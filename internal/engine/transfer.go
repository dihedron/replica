package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dihedron/veho/internal/policy"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5"
)

// TransferInput is the input of a Transfer workflow: IDs only (SEC-7).
type TransferInput struct {
	Tenant   uuid.UUID `json:"tenant"`
	Job      uuid.UUID `json:"job"`
	Transfer uuid.UUID `json:"transfer"`
}

// TransferWorkflowID is the workflow ID of a transfer.
func TransferWorkflowID(t uuid.UUID) string { return "transfer/" + t.String() }

// The caps on repeated failures (§5).
const (
	maxUnknown   = 10
	maxIntegrity = 3
)

// sourceInfo is jobs.source_info (§4.4).
type sourceInfo struct {
	Size     int64  `json:"size"`
	Version  string `json:"version,omitempty"`
	PartSize int64  `json:"part_size"`
	Digest   string `json:"digest,omitempty"`
}

// attempt is what plan fixes for one attempt.
type attempt struct {
	sourceInfo
	Source, Dest         uuid.UUID
	SourcePath, DestPath string
	Overwrite            string
	Resumable, Parallel  bool
}

type planResult struct {
	result
	Attempt attempt `json:"attempt"`
}

type noteResult struct {
	result
	Initial, MaxInterval time.Duration
}

// runStep runs fn as the step name. A DBOS error (the workflow cancelled,
// this executor fenced by DBOS, a shutdown) ends the workflow.
func runStep[R any](ctx dbos.Context, name string, fn func(context.Context) R) (R, error) {
	return dbos.RunAsStep(ctx, func(c context.Context) (R, error) { return fn(c), nil }, dbos.WithStepName(name))
}

// transferWorkflow is Transfer (§4.1): attempts until the transfer is
// terminal, waiting durably between them.
func (e *Engine) transferWorkflow(ctx dbos.Context, in TransferInput) (string, error) {
	defer e.running(ctx)()
	unknown, integrity := 0, 0
	for n := 0; ; n++ {
		r, err := e.attempt(ctx, in)
		if err != nil {
			return "", err
		}
		if r.Done || !r.failed() {
			return "done", nil
		}
		if r.Class == plugin.Unknown {
			unknown++
		} else {
			unknown = 0
		}
		switch {
		case r.Class == plugin.Transient, r.Class == plugin.Unknown && unknown < maxUnknown:
		case r.Class == plugin.ResumeInvalid, r.Class == plugin.Integrity && integrity < maxIntegrity-1:
			if r.Class == plugin.Integrity {
				integrity++
			}
			if _, err := runStep(ctx, "reset", func(c context.Context) result { return e.reset(c, in) }); err != nil {
				return "", err
			}
			continue
		default:
			_, err := runStep(ctx, "fail", func(c context.Context) result { return e.failTransfer(c, in, r) })
			return "failed", err
		}
		w, err := runStep(ctx, "wait", func(c context.Context) noteResult { return e.note(c, in, r) })
		if err != nil {
			return "", err
		}
		if w.Done {
			return "done", nil
		}
		if w.failed() { // the deadline
			_, err := runStep(ctx, "fail", func(c context.Context) result { return e.failTransfer(c, in, w.result) })
			return "failed", err
		}
		if _, err := dbos.Sleep(ctx, backoff(in.Transfer.String(), n, w.Initial, w.MaxInterval)); err != nil {
			return "", err
		}
	}
}

// attempt runs the steps of one attempt and returns the first that did not
// succeed, or the last.
func (e *Engine) attempt(ctx dbos.Context, in TransferInput) (result, error) {
	p, err := runStep(ctx, "plan", func(c context.Context) planResult { return e.plan(c, in) })
	if err != nil || p.failed() || p.Done {
		return p.result, err
	}
	a := p.Attempt
	steps := []struct {
		name string
		fn   func(context.Context) result
	}{{"begin", func(c context.Context) result { return e.begin(c, in, a) }}}
	if !a.Resumable { // the whole upload in one step: it cannot outlive it
		steps = []struct {
			name string
			fn   func(context.Context) result
		}{{"copy", func(c context.Context) result { return e.copyAll(c, in, a) }}}
	}
	for _, s := range steps {
		if r, err := runStep(ctx, s.name, s.fn); err != nil || r.failed() || r.Done {
			return r, err
		}
	}
	for a.Resumable {
		r, err := runStep(ctx, "batch", func(c context.Context) result { return e.batch(c, in, a, nil) })
		if err != nil || r.failed() || r.Done {
			return r, err
		}
		if !r.More {
			break
		}
	}
	for _, s := range []struct {
		name string
		fn   func(context.Context) result
	}{
		{"commit", func(c context.Context) result { return e.commit(c, in, a, nil) }},
		{"verify", func(c context.Context) result { return e.verify(c, in, a) }},
		{"finish", func(c context.Context) result { return e.finish(c, in) }},
	} {
		if !a.Resumable && s.name == "commit" {
			continue // copyAll committed
		}
		if r, err := runStep(ctx, s.name, s.fn); err != nil || r.failed() || r.Done {
			return r, err
		}
	}
	return result{Done: true}, nil
}

// errLost is a guarded write that found another fence: another executor
// holds the transfer, or it ended.
var errLost = errors.New("engine: the transfer is held elsewhere or ended")

var terminal = []string{string(policy.Succeeded), string(policy.Failed), string(policy.Cancelled)}

// claim takes or renews the lease and returns the fence (§4.2); live is
// false when the transfer is terminal.
func (e *Engine) claim(ctx context.Context, in TransferInput) (fence int64, live bool, err error) {
	err = store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `update veho.transfers
			set fence = case when lease_owner = $2 then fence else fence + 1 end,
			    lease_owner = $2, lease_expires_at = now() + make_interval(secs => $3)
			where id = $1 and state <> all($4) returning fence`,
			in.Transfer, e.executor, e.cfg.Engine.LeaseTTL.Seconds(), terminal).Scan(&fence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		live = err == nil
		return err
	})
	return fence, live, err
}

// guarded runs fn in a transaction that first checks the fence and that the
// transfer is not terminal, holding its row; errLost otherwise (INV-3).
func (e *Engine) guarded(ctx context.Context, in TransferInput, fence int64, fn func(pgx.Tx) error) error {
	return store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "select 1 from veho.transfers where id = $1 and fence = $2 and state <> all($3) for update", in.Transfer, fence, terminal)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLost
		}
		return fn(tx)
	})
}

// held runs fn under a claim and the node's fencing guard; a transfer that
// ended or moved to another executor gives Done.
func (e *Engine) held(ctx context.Context, in TransferInput, fn func(ctx context.Context, fence int64) result) result {
	gctx, release, err := e.guard(ctx)
	if err != nil {
		return fail(err)
	}
	defer release()
	fence, live, err := e.claim(gctx, in)
	if err != nil {
		return fail(err)
	}
	if !live {
		e.abandon(gctx, in)
		return result{Done: true}
	}
	return fn(gctx, fence)
}

// check turns a step's error into its result. A write that found another
// fence is retried, not taken for the end of the transfer: two executions of
// the workflow can overlap (a recovery during a failover), and the one that
// lost the fence may be the one DBOS keeps; it claims again and continues
// from the recorded parts, while DBOS stops the other at its next step. A
// transfer that really ended is Done at the next claim (found by AC-5).
func check(err error) result {
	if err == nil {
		return result{}
	}
	if errors.Is(err, errLost) {
		return result{Err: errLost.Error(), Class: plugin.Transient}
	}
	return fail(err)
}

// plan fixes the attempt (§4.1): the job's source info, the endpoints, the
// destination's capabilities; the transfer is COPYING.
func (e *Engine) plan(ctx context.Context, in TransferInput) planResult {
	var a attempt
	r := e.held(ctx, in, func(ctx context.Context, fence int64) result {
		var (
			info       []byte
			spec       []byte
			destDriver string
		)
		err := e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `select j.source_info, j.policy, j.source_endpoint_id, j.source_path,
					t.destination_endpoint_id, t.dest_path, d.driver
				from veho.transfers t join veho.jobs j on j.id = t.job_id join veho.endpoints d on d.id = t.destination_endpoint_id
				where t.id = $1`, in.Transfer).Scan(&info, &spec, &a.Source, &a.SourcePath, &a.Dest, &a.DestPath, &destDriver); err != nil {
				return err
			}
			if err := json.Unmarshal(info, &a.sourceInfo); err != nil || a.PartSize <= 0 {
				return plugin.Wrapf(plugin.Permanent, "engine: the job's source is not resolved")
			}
			var p policy.Policy
			if err := json.Unmarshal(spec, &p); err != nil {
				return plugin.Wrapf(plugin.Permanent, "engine: the job's policy: %v", err)
			}
			a.Overwrite = p.Overwrite
			d, err := e.driver(destDriver)
			if err != nil {
				return err
			}
			a.Resumable, a.Parallel = d.Capabilities().ResumableUpload, d.Capabilities().ParallelParts
			_, err = tx.Exec(ctx, `update veho.transfers set state = 'COPYING', bytes_total = $2, attempt = attempt + 1,
				started_at = coalesce(started_at, now()) where id = $1`, in.Transfer, a.Size)
			return err
		})
		return check(err)
	})
	return planResult{result: r, Attempt: a}
}

// begin starts the upload, or resumes it from its token (§4.1).
func (e *Engine) begin(ctx context.Context, in TransferInput, a attempt) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		up, err := e.upload(ctx, in, a, fence)
		if errors.Is(err, errSkip) {
			return e.end(ctx, in, fence, policy.Succeeded, nil)
		}
		if err != nil {
			return check(err)
		}
		up.close()
		return result{}
	})
}

// upload returns the transfer's upload: resumed from the stored token, or
// begun (checking overwrite first) and its token stored. The client stays
// open until ctx's step ends; the caller closes it through the returned
// upload's client.
func (e *Engine) upload(ctx context.Context, in TransferInput, a attempt, fence int64) (*openUpload, error) {
	_, dest, err := e.open(ctx, in.Tenant, a.Dest)
	if err != nil {
		return nil, err
	}
	ref := plugin.Ref{Key: a.DestPath}
	var token []byte
	var parts int
	if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "select decode(resume_token #>> '{}', 'base64') from veho.transfers where id = $1", in.Transfer).Scan(&token); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select count(*) from veho.parts where transfer_id = $1", in.Transfer).Scan(&parts)
	}); err != nil {
		_ = dest.Close()
		return nil, err
	}
	if token != nil {
		up, err := dest.Resume(ctx, ref, token)
		if err == nil {
			return &openUpload{Upload: up, client: dest}, nil
		}
		if plugin.ClassOf(err) != plugin.ResumeInvalid {
			_ = dest.Close()
			return nil, drv(err)
		}
		slog.WarnContext(ctx, "the upload is gone: beginning again", "tenant_id", in.Tenant.String(), "transfer_id", in.Transfer.String())
		parts = 0
	}
	if token == nil && parts == 0 && a.Overwrite != "replace" {
		if info, err := dest.Stat(ctx, ref); err == nil {
			_ = dest.Close()
			if a.Overwrite == "skip" && info.Size == a.Size {
				return nil, errSkip
			}
			return nil, plugin.Wrapf(plugin.Permanent, "engine: the destination object exists (overwrite: %s)", a.Overwrite)
		} else if !errors.Is(err, plugin.ErrNotFound) {
			_ = dest.Close()
			return nil, drv(err)
		}
	}
	up, err := dest.Begin(ctx, ref, plugin.ObjectInfo{Size: a.Size}, plugin.PutOptions{})
	if err != nil {
		_ = dest.Close()
		return nil, drv(err)
	}
	if err := e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "delete from veho.parts where transfer_id = $1", in.Transfer); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "update veho.transfers set resume_token = to_jsonb(encode($2::bytea, 'base64')), bytes_done = 0 where id = $1", in.Transfer, up.Token())
		return err
	}); err != nil {
		_ = up.Abort(ctx)
		_ = dest.Close()
		return nil, err
	}
	return &openUpload{Upload: up, client: dest}, nil
}

// errSkip: overwrite skip found the object in place.
var errSkip = errors.New("engine: the destination object is in place (overwrite: skip)")

type openUpload struct {
	plugin.Upload
	client plugin.Client
}

func (u *openUpload) close() { _ = u.client.Close() }

// batch copies the next parts_in_flight parts not yet committed and records
// them (§4.1); with up set, it copies into that upload and leaves it open.
func (e *Engine) batch(ctx context.Context, in TransferInput, a attempt, up *openUpload) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		if stop, err := e.mustStop(ctx, in); err != nil || stop {
			if err != nil {
				return fail(err)
			}
			return e.cancelTransfer(ctx, in, fence)
		}
		var done []int
		if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, "select part_no from veho.parts where transfer_id = $1", in.Transfer)
			if err != nil {
				return err
			}
			done, err = pgx.CollectRows(rows, pgx.RowTo[int])
			return err
		}); err != nil {
			return fail(err)
		}
		var todo []part
		for _, p := range plan(a.Size, a.PartSize) {
			if !slices.Contains(done, p.No) {
				todo = append(todo, p)
			}
		}
		if len(todo) == 0 {
			return result{}
		}
		more := len(todo) > e.cfg.Engine.PartsInFlight
		todo = todo[:min(len(todo), e.cfg.Engine.PartsInFlight)]
		if up == nil {
			var err error
			if up, err = e.upload(ctx, in, a, fence); err != nil {
				return check(err)
			}
			defer up.close()
		}
		_, src, err := e.open(ctx, in.Tenant, a.Source)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = src.Close() }()
		copied, cerr := e.copyParts(ctx, src, up, a, todo)
		werr := e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			for _, c := range copied {
				if _, err := tx.Exec(ctx, `insert into veho.parts (tenant_id, transfer_id, part_no, "offset", length, sha256, etag)
					values (veho.current_tenant(), $1, $2, $3, $4, $5, $6) on conflict (transfer_id, part_no) do nothing`,
					in.Transfer, c.part.No, c.part.Offset, c.part.Length, c.sum, c.etag); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `update veho.transfers set bytes_done = (select coalesce(sum(length), 0) from veho.parts where transfer_id = $1)
				where id = $1`, in.Transfer)
			return err
		})
		if werr == nil {
			for _, c := range copied {
				e.metrics.bytes.Add(float64(c.part.Length))
			}
		}
		if err := errors.Join(cerr, werr); err != nil {
			return check(err)
		}
		return result{More: more}
	})
}

type copiedPart struct {
	part part
	sum  []byte
	etag string
}

// copyParts streams the parts from the source into the upload, computing
// each part's SHA-256 on the read path (FR-8, PLG-17); it returns the parts
// that succeeded and the first error.
func (e *Engine) copyParts(ctx context.Context, src plugin.Client, up plugin.Upload, a attempt, todo []part) ([]copiedPart, error) {
	var (
		mu     sync.Mutex
		copied []copiedPart
		errs   []error
		wg     sync.WaitGroup
	)
	one := func(p part) {
		c, err := e.copyPart(ctx, src, up, a, p)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, err)
			return
		}
		copied = append(copied, c)
	}
	for _, p := range todo {
		if a.Parallel {
			wg.Go(func() { one(p) })
		} else {
			one(p)
		}
	}
	wg.Wait()
	if len(errs) > 0 {
		return copied, errs[0]
	}
	return copied, nil
}

// copyPart copies one part once the node's part memory has room for it: a
// driver may hold the whole part in memory (NFR-5). A part larger than the
// whole budget takes all of it.
func (e *Engine) copyPart(ctx context.Context, src plugin.Client, up plugin.Upload, a attempt, p part) (copiedPart, error) {
	n := min(p.Length, int64(e.cfg.Engine.PartMemoryMiB)<<20)
	if err := e.partMemory.Acquire(ctx, n); err != nil {
		return copiedPart{}, fmt.Errorf("wait for part memory: %w", err)
	}
	defer e.partMemory.Release(n)
	rc, err := src.ReadRange(ctx, plugin.Ref{Key: a.SourcePath}, p.Offset, p.Length, a.Version)
	if err != nil {
		return copiedPart{}, drv(fmt.Errorf("read part %d: %w", p.No, err))
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	res, err := up.WritePart(ctx, p.No, io.TeeReader(rc, h), p.Length)
	if err != nil {
		return copiedPart{}, drv(fmt.Errorf("write part %d: %w", p.No, err))
	}
	sum := h.Sum(nil)
	if res.Size != p.Length {
		return copiedPart{}, plugin.Wrapf(plugin.Integrity, "part %d: %d bytes written, want %d", p.No, res.Size, p.Length)
	}
	if res.SHA256 != "" && res.SHA256 != hex.EncodeToString(sum) {
		return copiedPart{}, plugin.Wrapf(plugin.Integrity, "part %d: the driver wrote other bytes than were read", p.No)
	}
	return copiedPart{part: p, sum: sum, etag: res.ETag}, nil
}

// copyAll is begin, every batch and commit in one step, for a driver whose
// uploads cannot be resumed from another step (§4.1).
func (e *Engine) copyAll(ctx context.Context, in TransferInput, a attempt) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		if err := e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "delete from veho.parts where transfer_id = $1", in.Transfer)
			return err
		}); err != nil {
			return check(err)
		}
		up, err := e.upload(ctx, in, a, fence)
		if errors.Is(err, errSkip) {
			return e.end(ctx, in, fence, policy.Succeeded, nil)
		}
		if err != nil {
			return check(err)
		}
		defer up.close()
		if p, ok := up.Upload.(plugin.Puller); ok && p.PullTTL() > 0 {
			if err := e.pull(ctx, in, a, fence, up, p); err != nil {
				_ = up.Abort(ctx)
				return check(err)
			}
			return e.commit(ctx, in, a, up)
		}
		for {
			r := e.batch(ctx, in, a, up)
			if r.failed() || r.Done {
				_ = up.Abort(ctx)
				return r
			}
			if !r.More {
				return e.commit(ctx, in, a, up)
			}
		}
	})
}

// pull is the copy of a destination that fetches the object itself from a
// URL (Glance's web-download, veho-openstack.md §3.2): the bytes do not cross
// veho, so the job's digest is first recorded from the source when no other
// destination has, and the pulled object, still invisible, is read back by
// parts, whose sums are the transfer's parts; verify then compares them with
// the source's before Commit, and again by verify as for every destination
// (FR-8).
func (e *Engine) pull(ctx context.Context, in TransferInput, a attempt, fence int64, up *openUpload, p plugin.Puller) error {
	_, src, err := e.open(ctx, in.Tenant, a.Source)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	var known string
	if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select coalesce(source_info->>'digest', '') from veho.jobs where id = $1", in.Job).Scan(&known)
	}); err != nil {
		return err
	}
	// the digest is recorded unless another destination recorded one meanwhile
	sums, err := pullChecked(ctx, src, up.client, p, a, known, func(d string) (recorded string, err error) {
		return recorded, e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `update veho.jobs set source_info = case when source_info ? 'digest' then source_info
				else jsonb_set(source_info, '{digest}', to_jsonb($2::text)) end where id = $1 returning source_info->>'digest'`, in.Job, d).Scan(&recorded)
		})
	})
	if err != nil {
		return err
	}
	e.metrics.bytes.Add(float64(a.Size))
	// the parts as plan cuts them: part i at i * part size
	return e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `with p as (insert into veho.parts (tenant_id, transfer_id, part_no, "offset", length, sha256)
				select veho.current_tenant(), $1, ord - 1, $3 * (ord - 1), least($3, $2 - $3 * (ord - 1)), s
				from unnest($4::bytea[]) with ordinality as p(s, ord)
				on conflict (transfer_id, part_no) do nothing)
			update veho.transfers set bytes_done = $2 where id = $1`, in.Transfer, a.Size, a.PartSize, sums)
		return err
	})
}

// pullChecked is pull without the database: the job's digest from the
// source when none is recorded (record stores it and returns the one
// recorded), the URL, the pull, and the read-back compared with the job's
// digest before Commit, so a wrong copy is never published (OS-5). It
// returns the parts' sums of the copy.
func pullChecked(ctx context.Context, src, dst plugin.Client, p plugin.Puller, a attempt, recorded string, record func(string) (string, error)) ([][]byte, error) {
	presigner, ok := src.(plugin.Presigner)
	if !ok {
		return nil, plugin.Wrapf(plugin.Permanent, "engine: the source cannot give the URL a pulling destination needs")
	}
	srcRef := plugin.Ref{Key: a.SourcePath}
	if recorded == "" {
		sums, err := readSums(ctx, src, srcRef, a, a.Version)
		if err != nil {
			return nil, err
		}
		d, err := digest(a.PartSize, a.Size, sums)
		if err != nil {
			return nil, plugin.Wrap(err, plugin.Integrity)
		}
		if recorded, err = record(d); err != nil {
			return nil, err
		}
	}
	u, err := presigner.PresignRead(ctx, srcRef, a.Version, p.PullTTL())
	if err != nil {
		return nil, drv(fmt.Errorf("presign the source: %w", err))
	}
	ref, err := p.Pull(ctx, u)
	if err != nil {
		return nil, drv(fmt.Errorf("pull: %w", err))
	}
	sums, err := readSums(ctx, dst, ref, a, "")
	if err != nil {
		return nil, fmt.Errorf("read the pulled object back: %w", err)
	}
	d, err := digest(a.PartSize, a.Size, sums)
	if err != nil {
		return nil, plugin.Wrap(err, plugin.Integrity)
	}
	if d != recorded {
		return nil, plugin.Wrapf(plugin.Integrity, "engine: the pulled copy's digest differs from the source's")
	}
	return sums, nil
}

// readSums reads every part of ref by ranges and returns their SHA-256s.
func readSums(ctx context.Context, c plugin.Client, ref plugin.Ref, a attempt, version string) ([][]byte, error) {
	parts := plan(a.Size, a.PartSize)
	sums := make([][]byte, len(parts))
	for i, p := range parts {
		rc, err := c.ReadRange(ctx, ref, p.Offset, p.Length, version)
		if err != nil {
			return nil, drv(fmt.Errorf("read part %d: %w", p.No, err))
		}
		h := sha256.New()
		n, err := io.Copy(h, rc)
		_ = rc.Close()
		if err != nil {
			return nil, drv(fmt.Errorf("read part %d: %w", p.No, err))
		}
		if n != p.Length {
			return nil, plugin.Wrapf(plugin.Integrity, "part %d: %d bytes read, want %d", p.No, n, p.Length)
		}
		sums[i] = h.Sum(nil)
	}
	return sums, nil
}

// commit publishes the object (FR-9); the transfer is VERIFYING.
func (e *Engine) commit(ctx context.Context, in TransferInput, a attempt, up *openUpload) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		parts, err := e.parts(ctx, in)
		if err != nil {
			return fail(err)
		}
		if len(parts) != len(plan(a.Size, a.PartSize)) {
			return fail(fmt.Errorf("engine: %d parts recorded, %d planned", len(parts), len(plan(a.Size, a.PartSize))))
		}
		if up == nil {
			if up, err = e.upload(ctx, in, a, fence); err != nil {
				return check(err)
			}
			defer up.close()
		}
		results := make([]plugin.PartResult, len(parts))
		for i, p := range parts {
			results[i] = plugin.PartResult{PartNo: p.No, Size: p.Length, SHA256: hex.EncodeToString(p.SHA256)}
			if p.ETag != nil {
				results[i].ETag = *p.ETag
			}
		}
		if _, err := up.Commit(ctx, results); err != nil {
			return fail(drv(fmt.Errorf("commit: %w", err)))
		}
		return check(e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "update veho.transfers set state = 'VERIFYING' where id = $1", in.Transfer)
			return err
		}))
	})
}

type partRow struct {
	No     int
	Length int64
	SHA256 []byte
	ETag   *string
}

// parts are the recorded parts, in order.
func (e *Engine) parts(ctx context.Context, in TransferInput) ([]partRow, error) {
	var rows []partRow
	err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, "select part_no, length, sha256, etag from veho.parts where transfer_id = $1 order by part_no", in.Transfer)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, pgx.RowToStructByPos[partRow])
		return err
	})
	return rows, err
}

// verify checks the destination's size and the transfer's digest against
// the job's, recording the job's digest if it is the first (§4.4, FR-8).
func (e *Engine) verify(ctx context.Context, in TransferInput, a attempt) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		_, dest, err := e.open(ctx, in.Tenant, a.Dest)
		if err != nil {
			return fail(err)
		}
		info, err := dest.Stat(ctx, plugin.Ref{Key: a.DestPath})
		_ = dest.Close()
		if err != nil {
			return fail(drv(fmt.Errorf("stat the destination: %w", err)))
		}
		if info.Size != a.Size {
			return fail(plugin.Wrapf(plugin.Integrity, "engine: the destination has %d bytes, the source %d", info.Size, a.Size))
		}
		parts, err := e.parts(ctx, in)
		if err != nil {
			return fail(err)
		}
		sums := make([][]byte, len(parts))
		for i, p := range parts {
			sums[i] = p.SHA256
		}
		d, err := digest(a.PartSize, a.Size, sums)
		if err != nil {
			return fail(plugin.Wrap(err, plugin.Integrity))
		}
		var recorded string
		err = e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `update veho.jobs set source_info = case when source_info ? 'digest' then source_info
					else jsonb_set(source_info, '{digest}', to_jsonb($2::text)) end
				where id = $1 returning source_info->>'digest'`, in.Job, d).Scan(&recorded)
		})
		if err != nil {
			return check(err)
		}
		if recorded != d {
			return fail(plugin.Wrapf(plugin.Integrity, "engine: the copy's digest differs from the one recorded for the job"))
		}
		return result{}
	})
}

// finish marks the transfer SUCCEEDED and decides the job (§4.3).
func (e *Engine) finish(ctx context.Context, in TransferInput) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		return e.end(ctx, in, fence, policy.Succeeded, nil)
	})
}

// end moves the transfer to a terminal state, releases its lease and
// decides the job, in one transaction.
func (e *Engine) end(ctx context.Context, in TransferInput, fence int64, state policy.State, lastErr *result) result {
	var errJSON []byte
	if lastErr != nil {
		errJSON, _ = json.Marshal(map[string]string{"class": lastErr.Class.String(), "message": lastErr.Err})
	}
	err := e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update veho.transfers set state = $2, finished_at = now(), lease_owner = null, lease_expires_at = null,
			last_error = coalesce($3::jsonb, last_error), resume_token = case when $2 = 'SUCCEEDED' then null else resume_token end
			where id = $1`, in.Transfer, string(state), errJSON); err != nil {
			return err
		}
		return e.decide(ctx, tx, in.Job)
	})
	if err != nil {
		return check(err)
	}
	return result{Done: true}
}

// failTransfer ends the transfer FAILED with r as its last error, aborting
// its upload.
func (e *Engine) failTransfer(ctx context.Context, in TransferInput, r result) result {
	e.metrics.attempts.WithLabelValues(r.Class.String()).Inc()
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		e.abort(ctx, in)
		return e.end(ctx, in, fence, policy.Failed, &r)
	})
}

// cancelTransfer ends the transfer CANCELLED, aborting its upload.
func (e *Engine) cancelTransfer(ctx context.Context, in TransferInput, fence int64) result {
	e.abort(ctx, in)
	return e.end(ctx, in, fence, policy.Cancelled, nil)
}

// mustStop is true when the transfer must stop: the job cancelled, failed,
// or satisfied with on_satisfied stop.
func (e *Engine) mustStop(ctx context.Context, in TransferInput) (bool, error) {
	var stop bool
	err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select cancel_requested or state = 'FAILED' or state = 'CANCELLED'
			or (state = 'SUCCEEDED' and policy->>'on_satisfied' = 'stop') from veho.jobs where id = $1`, in.Job).Scan(&stop)
	})
	return stop, err
}

// abort discards the transfer's upload, if any, as best it can: an upload
// left behind only costs the destination's staging space.
func (e *Engine) abort(ctx context.Context, in TransferInput) {
	var (
		dest  uuid.UUID
		path  string
		token []byte
	)
	if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select destination_endpoint_id, dest_path, decode(resume_token #>> '{}', 'base64')
			from veho.transfers where id = $1`, in.Transfer).Scan(&dest, &path, &token)
	}); err != nil || token == nil {
		return
	}
	_, c, err := e.open(ctx, in.Tenant, dest)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	if up, err := c.Resume(ctx, plugin.Ref{Key: path}, token); err == nil {
		if err := up.Abort(ctx); err != nil {
			slog.WarnContext(ctx, "cannot abort the upload", "tenant_id", in.Tenant.String(), "transfer_id", in.Transfer.String(), "error", sanitize(err.Error()))
		}
	}
	_ = store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "update veho.transfers set resume_token = null where id = $1", in.Transfer)
		return err
	})
}

// abandon aborts the upload of a transfer found terminal at a claim (it was
// cancelled while this workflow waited).
func (e *Engine) abandon(ctx context.Context, in TransferInput) {
	var state string
	if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select state from veho.transfers where id = $1", in.Transfer).Scan(&state)
	}); err == nil && state != string(policy.Succeeded) {
		e.abort(ctx, in)
	}
}

// reset discards the attempt's upload, parts and digest, and the object if
// this transfer committed it, so that the next attempt starts from scratch
// (ResumeInvalid, Integrity: §5).
func (e *Engine) reset(ctx context.Context, in TransferInput) result {
	return e.held(ctx, in, func(ctx context.Context, fence int64) result {
		var (
			state, path string
			dest        uuid.UUID
		)
		if err := store.InTenant(ctx, e.worker, in.Tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "select state, dest_path, destination_endpoint_id from veho.transfers where id = $1", in.Transfer).Scan(&state, &path, &dest)
		}); err != nil {
			return fail(err)
		}
		e.abort(ctx, in)
		if state == string(policy.Verifying) { // this transfer published it
			_, c, err := e.open(ctx, in.Tenant, dest)
			if err != nil {
				return fail(err)
			}
			err = c.Delete(ctx, plugin.Ref{Key: path})
			_ = c.Close()
			if err != nil && !errors.Is(err, plugin.ErrNotFound) {
				return fail(drv(err))
			}
		}
		return check(e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "delete from veho.parts where transfer_id = $1", in.Transfer); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "update veho.transfers set state = 'PENDING', resume_token = null, bytes_done = 0 where id = $1", in.Transfer)
			return err
		}))
	})
}

// note records the failure before the wait (RETRY_WAIT, last_error) and
// returns the policy's backoff; past the job's deadline it fails (§5).
func (e *Engine) note(ctx context.Context, in TransferInput, r result) noteResult {
	e.metrics.attempts.WithLabelValues(r.Class.String()).Inc()
	out := noteResult{}
	out.result = e.held(ctx, in, func(ctx context.Context, fence int64) result {
		errJSON, _ := json.Marshal(map[string]string{"class": r.Class.String(), "message": r.Err})
		var (
			late bool
			spec []byte
		)
		err := e.guarded(ctx, in, fence, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "select now() > deadline_at, policy from veho.jobs where id = $1", in.Job).Scan(&late, &spec); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "update veho.transfers set state = 'RETRY_WAIT', last_error = $2 where id = $1", in.Transfer, errJSON)
			return err
		})
		if err != nil {
			return check(err)
		}
		if late {
			return result{Err: "deadline passed; last error: " + r.Err, Class: plugin.Permanent}
		}
		var p policy.Policy
		_ = json.Unmarshal(spec, &p)
		p = p.Normalize()
		out.Initial, out.MaxInterval = time.Duration(p.Retry.Initial), time.Duration(p.Retry.MaxInterval)
		return result{}
	})
	if out.Initial == 0 { // a note that failed still waits
		out.Initial, out.MaxInterval = 2*time.Second, 15*time.Minute
	}
	if out.failed() && out.Class == plugin.Transient {
		out.result = result{}
	}
	return out
}
