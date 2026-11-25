// Package conformance is the suite every storage driver must pass
// (.specs/veho-plugins.md §4 and §7), built-in or third-party, under every
// loader.
package conformance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
)

// Target is the driver under test and an endpoint it can write to.
type Target struct {
	Driver      plugin.Driver
	Config      plugin.Config
	Credentials plugin.Credentials
	// BadCredentials, when set, must make the first call fail as Permanent.
	BadCredentials plugin.Credentials
	// Container is the Ref.Container of the objects the suite writes.
	Container string
	// Prefix starts every key the suite writes.
	Prefix string
	// PartSize is the size of the parts (default: the driver's minimum part
	// size, at least 1 MiB).
	PartSize int64
	// Seed of the random sizes and contents (default: the time); a failure
	// reports it.
	Seed uint64
	// Put stores data as ref behind the driver's back, for a driver that is
	// a source only (veho-sources.md SRC-4): the suite then runs its source
	// cases, writing the objects through Put. It replaces an object that is
	// there, and removes the objects in t's cleanup.
	Put func(t *testing.T, ref plugin.Ref, data []byte)
	// Confined: the driver refuses keys that leave its root (SRC-W4), which
	// the suite checks with "..", absolute and encoded keys.
	Confined bool
}

// suite holds the state of one run.
type suite struct {
	Target
	src *rand.ChaCha8
	rnd *rand.Rand
	mu  sync.Mutex
}

// Run runs the suite as subtests of t.
func Run(t *testing.T, tg Target) {
	if tg.Seed == 0 {
		tg.Seed = uint64(time.Now().UnixNano())
	}
	caps := tg.Driver.Capabilities()
	if tg.PartSize == 0 {
		tg.PartSize = max(caps.MinPartSize, 1<<20)
	}
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], tg.Seed)
	src := rand.NewChaCha8(seed)
	// seeded on purpose: a failure is reproduced from the seed it reports
	s := &suite{Target: tg, src: src, rnd: rand.New(src)} //nolint:gosec // test data, not secrets
	t.Logf("conformance: driver %s, seed %d, part size %d", tg.Driver.Name(), tg.Seed, tg.PartSize)
	t.Run("Capabilities", s.capabilities)
	if tg.Confined {
		t.Run("Confined", s.confined)
	}
	if caps.Source && !caps.Sink && tg.Put != nil {
		t.Run("ZeroBytes", s.zeroBytes)
		t.Run("RoundTrip", s.roundTrip)
		t.Run("SourceChanged", s.sourceChanged)
		t.Run("NotFound", s.notFound)
		t.Run("Cancellation", s.cancellation)
		t.Run("BadCredentials", s.badCredentials)
		return
	}
	if !caps.Sink || !caps.Source {
		t.Skip("conformance: the suite needs a driver that is both source and sink, or a Put for a source")
	}
	t.Run("ZeroBytes", s.zeroBytes)
	t.Run("RoundTrip", s.roundTrip)
	t.Run("InvisibleBeforeCommit", s.invisibleBeforeCommit)
	t.Run("Idempotent", s.idempotent)
	t.Run("Resume", s.resume)
	t.Run("ResumeInvalid", s.resumeInvalid)
	t.Run("SourceChanged", s.sourceChanged)
	t.Run("NotFound", s.notFound)
	t.Run("Delete", s.delete)
	t.Run("Cancellation", s.cancellation)
	t.Run("BadCredentials", s.badCredentials)
}

// --- helpers -----------------------------------------------------------------

func (s *suite) open(t *testing.T) plugin.Client {
	t.Helper()
	c, err := s.Driver.Open(context.Background(), s.Config, s.Credentials)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (s *suite) ref(name string) plugin.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	return plugin.Ref{Container: s.Container, Key: fmt.Sprintf("%s%s-%016x", s.Prefix, name, s.rnd.Uint64())}
}

func (s *suite) bytes(n int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := make([]byte, n)
	_, _ = s.src.Read(b) // never fails
	return b
}

func (s *suite) intn(n int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rnd.Int64N(n)
}

// parts splits data into parts of PartSize, the last one shorter (at least
// one part, empty for empty data).
func (s *suite) parts(data []byte) [][]byte {
	var out [][]byte
	for off := int64(0); off < int64(len(data)); off += s.PartSize {
		out = append(out, data[off:min(off+s.PartSize, int64(len(data)))])
	}
	if len(out) == 0 {
		out = [][]byte{{}}
	}
	return out
}

// put stores data as ref: through Put for a source-only driver, else
// through the driver.
func (s *suite) put(t *testing.T, c plugin.Client, ref plugin.Ref, data []byte) {
	t.Helper()
	if s.Put != nil && !s.Driver.Capabilities().Sink {
		s.Put(t, ref, data)
		return
	}
	s.write(t, c, ref, data)
}

// write uploads data as ref, in parts (concurrently when the driver allows
// it), and commits.
func (s *suite) write(t *testing.T, c plugin.Client, ref plugin.Ref, data []byte) plugin.ObjectInfo {
	t.Helper()
	ctx := context.Background()
	up, err := c.Begin(ctx, ref, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		t.Fatalf("Begin(%v): %v", ref, err)
	}
	results := s.writeParts(t, up, s.parts(data), 0)
	info, err := up.Commit(ctx, results)
	if err != nil {
		t.Fatalf("Commit(%v): %v", ref, err)
	}
	// before the client closes (cleanups run last in, first out)
	t.Cleanup(func() { _ = c.Delete(context.Background(), ref) })
	if info.Size != int64(len(data)) {
		t.Fatalf("Commit(%v): size %d, want %d", ref, info.Size, len(data))
	}
	return info
}

// writeParts writes parts numbered from first, and returns their results by
// part number.
func (s *suite) writeParts(t *testing.T, up plugin.Upload, parts [][]byte, first int) []plugin.PartResult {
	t.Helper()
	results := make([]plugin.PartResult, len(parts))
	errs := make([]error, len(parts))
	write := func(i int) {
		results[i], errs[i] = up.WritePart(context.Background(), first+i, bytes.NewReader(parts[i]), int64(len(parts[i])))
	}
	if s.Driver.Capabilities().ParallelParts {
		var wg sync.WaitGroup
		for i := len(parts) - 1; i >= 0; i-- { // out of order
			wg.Go(func() { write(i) })
		}
		wg.Wait()
	} else {
		for i := range parts {
			write(i)
		}
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WritePart(%d): %v", first+i, err)
		}
		if results[i].PartNo != first+i || results[i].Size != int64(len(parts[i])) {
			t.Fatalf("WritePart(%d) = %+v", first+i, results[i])
		}
	}
	return results
}

// read reads [off, off+n) of ref.
func (s *suite) read(t *testing.T, c plugin.Client, ref plugin.Ref, off, n int64, version string) []byte {
	t.Helper()
	r, err := c.ReadRange(context.Background(), ref, off, n, version)
	if err != nil {
		t.Fatalf("ReadRange(%v, %d, %d): %v", ref, off, n, err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadRange(%v, %d, %d): read: %v", ref, off, n, err)
	}
	return b
}

func wantClass(t *testing.T, what string, err error, class plugin.Class) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want class %v", what, class)
	} else if got := plugin.ClassOf(err); got != class {
		t.Errorf("%s: class %v (%v), want %v", what, got, err, class)
	}
}

// --- the cases ---------------------------------------------------------------

func (s *suite) capabilities(t *testing.T) {
	caps := s.Driver.Capabilities()
	if caps.Source && !caps.RangeRead {
		t.Error("a source must read ranges")
	}
	if caps.MaxPartSize > 0 && caps.MinPartSize > caps.MaxPartSize {
		t.Errorf("MinPartSize %d > MaxPartSize %d", caps.MinPartSize, caps.MaxPartSize)
	}
	if caps.MaxPartSize > 0 && s.PartSize > caps.MaxPartSize || s.PartSize < caps.MinPartSize {
		t.Errorf("the suite's part size %d is outside [%d, %d]", s.PartSize, caps.MinPartSize, caps.MaxPartSize)
	}
	var schema map[string]any
	if err := json.Unmarshal(s.Driver.ConfigSchema(), &schema); err != nil || schema["type"] != "object" {
		t.Errorf("ConfigSchema is not a JSON Schema of an object: %v", err)
	}
	if s.Driver.Name() == "" || len(s.Driver.Schemes()) == 0 {
		t.Error("a driver has a name and at least one scheme")
	}
}

func (s *suite) zeroBytes(t *testing.T) {
	c := s.open(t)
	ref := s.ref("zero")
	s.put(t, c, ref, nil)
	info, err := c.Stat(context.Background(), ref)
	if err != nil || info.Size != 0 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if b := s.read(t, c, ref, 0, 0, info.Version); len(b) != 0 {
		t.Errorf("read %d bytes of an empty object", len(b))
	}
}

func (s *suite) roundTrip(t *testing.T) {
	c := s.open(t)
	sizes := []int64{1, s.PartSize, s.PartSize + 1, 2*s.PartSize + s.intn(s.PartSize)}
	for _, size := range sizes {
		ref := s.ref(fmt.Sprintf("rt%d", size))
		data := s.bytes(size)
		s.put(t, c, ref, data)
		info, err := c.Stat(context.Background(), ref)
		if err != nil || info.Size != size || info.Version == "" {
			t.Fatalf("size %d: Stat = %+v, %v", size, info, err)
		}
		if got := s.read(t, c, ref, 0, size, info.Version); !bytes.Equal(got, data) {
			t.Fatalf("size %d: the object read back differs", size)
		}
		// the last partial range, and random ranges
		last := size % s.PartSize
		if last == 0 {
			last = min(size, s.PartSize)
		}
		ranges := [][2]int64{{size - last, last}, {size - 1, 1}}
		for range 4 {
			off := s.intn(size)
			ranges = append(ranges, [2]int64{off, 1 + s.intn(size-off)})
		}
		for _, r := range ranges {
			if got := s.read(t, c, ref, r[0], r[1], ""); !bytes.Equal(got, data[r[0]:r[0]+r[1]]) {
				t.Errorf("size %d: range [%d, +%d) differs", size, r[0], r[1])
			}
		}
	}
}

func (s *suite) invisibleBeforeCommit(t *testing.T) {
	c := s.open(t)
	ctx := context.Background()
	ref := s.ref("invisible")
	up, err := c.Begin(ctx, ref, plugin.ObjectInfo{}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.writeParts(t, up, [][]byte{s.bytes(s.PartSize), s.bytes(10)}, 0)
	if _, err := c.Stat(ctx, ref); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("Stat before Commit: %v, want ErrNotFound", err)
	}
	if err := up.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := c.Stat(ctx, ref); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("Stat after Abort: %v, want ErrNotFound", err)
	}
	if err := up.Abort(ctx); err != nil {
		t.Errorf("Abort again: %v", err)
	}
}

func (s *suite) idempotent(t *testing.T) {
	c := s.open(t)
	ctx := context.Background()
	ref := s.ref("idempotent")
	data := s.bytes(s.PartSize + 7)
	parts := s.parts(data)
	up, err := c.Begin(ctx, ref, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.writeParts(t, up, parts[:1], 0)
	results := s.writeParts(t, up, parts, 0) // part 0 again
	t.Cleanup(func() { _ = c.Delete(context.Background(), ref) })
	first, err := up.Commit(ctx, results)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	second, err := up.Commit(ctx, results)
	if err != nil {
		t.Fatalf("Commit again: %v", err)
	}
	if first.Size != second.Size || first.Version != second.Version {
		t.Errorf("Commit again = %+v, first %+v", second, first)
	}
	if got := s.read(t, c, ref, 0, int64(len(data)), ""); !bytes.Equal(got, data) {
		t.Error("the object differs after a part was written twice")
	}
}

func (s *suite) resume(t *testing.T) {
	if !s.Driver.Capabilities().ResumableUpload {
		t.Skip("not resumable")
	}
	ctx := context.Background()
	ref := s.ref("resume")
	data := s.bytes(2*s.PartSize + 3)
	parts := s.parts(data)
	up, err := s.open(t).Begin(ctx, ref, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.writeParts(t, up, parts[:1], 0)
	token := up.Token()
	// another client, as another process after a crash
	c := s.open(t)
	again, err := c.Resume(ctx, ref, token)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	listed, err := again.ListParts(ctx)
	if err != nil || len(listed) != 1 || listed[0].PartNo != 0 || listed[0].Size != int64(len(parts[0])) {
		t.Fatalf("ListParts after Resume = %+v, %v", listed, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), ref) })
	rest := s.writeParts(t, again, parts[1:], 1)
	if _, err := again.Commit(ctx, append(listed, rest...)); err != nil {
		t.Fatalf("Commit after Resume: %v", err)
	}
	if got := s.read(t, c, ref, 0, int64(len(data)), ""); !bytes.Equal(got, data) {
		t.Error("the resumed object differs")
	}
}

func (s *suite) resumeInvalid(t *testing.T) {
	c := s.open(t)
	ctx := context.Background()
	ref := s.ref("resume-invalid")
	up, err := c.Begin(ctx, ref, plugin.ObjectInfo{}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token := up.Token()
	if err := up.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if again, err := c.Resume(ctx, ref, token); err == nil {
		// a driver may only find out at the next call
		_, err = again.WritePart(ctx, 0, bytes.NewReader([]byte("x")), 1)
		wantClass(t, "WritePart on an aborted upload", err, plugin.ResumeInvalid)
	} else {
		wantClass(t, "Resume of an aborted upload", err, plugin.ResumeInvalid)
	}
	_, err = c.Resume(ctx, ref, []byte("not a token"))
	wantClass(t, "Resume of a malformed token", err, plugin.ResumeInvalid)
}

func (s *suite) sourceChanged(t *testing.T) {
	c := s.open(t)
	ref := s.ref("changed")
	s.put(t, c, ref, s.bytes(100))
	before, err := c.Stat(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // a version may include a time
	s.put(t, c, ref, s.bytes(101))
	_, err = c.ReadRange(context.Background(), ref, 0, 10, before.Version)
	wantClass(t, "ReadRange of an old version", err, plugin.SourceChanged)
}

// confined: keys that leave the root are refused as Permanent, not looked
// up (SRC-W4); keys with encoded separators are at most absent, as a
// driver for which they are plain names finds no such object.
func (s *suite) confined(t *testing.T) {
	c := s.open(t)
	ctx := context.Background()
	for _, key := range []string{"../x", "a/../../x", "/etc/passwd", "..", "."} {
		ref := plugin.Ref{Container: s.Container, Key: key}
		if _, err := c.Stat(ctx, ref); err == nil || errors.Is(err, plugin.ErrNotFound) || plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("Stat(%q): %v, want a Permanent refusal", key, err)
		}
		if r, err := c.ReadRange(ctx, ref, 0, 1, ""); err == nil {
			_ = r.Close()
			t.Errorf("ReadRange(%q): no error", key)
		}
	}
	for _, key := range []string{"%2e%2e/x", "a/%2e%2e/%2e%2e/x", "..%2fx", "%2e%2e%2fx"} {
		ref := plugin.Ref{Container: s.Container, Key: key}
		if _, err := c.Stat(ctx, ref); err == nil || plugin.ClassOf(err) != plugin.Permanent {
			t.Errorf("Stat(%q): %v, want refused or absent", key, err)
		}
	}
}

func (s *suite) notFound(t *testing.T) {
	c := s.open(t)
	ref := s.ref("absent")
	if _, err := c.Stat(context.Background(), ref); !errors.Is(err, plugin.ErrNotFound) || plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("Stat of an absent object: %v (%v), want ErrNotFound, Permanent", err, plugin.ClassOf(err))
	}
	if _, err := c.ReadRange(context.Background(), ref, 0, 1, ""); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("ReadRange of an absent object: %v, want ErrNotFound", err)
	}
}

func (s *suite) delete(t *testing.T) {
	c := s.open(t)
	ctx := context.Background()
	ref := s.ref("delete")
	s.write(t, c, ref, s.bytes(10))
	if err := c.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Stat(ctx, ref); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("Stat after Delete: %v", err)
	}
	if err := c.Delete(ctx, ref); err != nil {
		t.Errorf("Delete of an absent object: %v", err)
	}
}

func (s *suite) cancellation(t *testing.T) {
	c := s.open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := c.Stat(ctx, s.ref("cancelled"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Stat with a cancelled context: %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stat with a cancelled context took %v", d)
	}
}

func (s *suite) badCredentials(t *testing.T) {
	if s.BadCredentials == nil {
		t.Skip("no bad credentials for this target")
	}
	c, err := s.Driver.Open(context.Background(), s.Config, s.BadCredentials)
	if err == nil {
		defer func() { _ = c.Close() }()
		_, err = c.Stat(context.Background(), s.ref("bad-credentials"))
	}
	wantClass(t, "a call with bad credentials", err, plugin.Permanent)
}
