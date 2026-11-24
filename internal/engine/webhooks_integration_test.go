//go:build integration

package engine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/webhook"
	"github.com/jackc/pgx/v5"
)

// receiver is a webhook endpoint of the test: it records what it gets and
// answers what the test says.
type receiver struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	got    []delivery
}

type delivery struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, delivery{req.Header.Clone(), b})
		st := r.status
		r.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) answer(status int) { r.mu.Lock(); r.status = status; r.mu.Unlock() }

func (r *receiver) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

// hookFixture is a fixture whose engine trusts the receivers' certificates.
func hookFixture(t *testing.T) fixture {
	t.Helper()
	f := setup(t)
	// the dispatcher serves every tenant: leave none of an earlier test's
	f.sql(t, "delete from veho.webhook_outbox")
	f.sql(t, "delete from veho.webhooks")
	f.e.hooks.WithTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the test's own receiver
	return f
}

// settle moves a new job of the tenant to state, as the engine does.
func (f fixture) settle(t *testing.T, state string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	job, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "src", SourcePath: "in/" + uuid.New().String(), Policy: []byte(twoCopies)})
	if err != nil {
		t.Fatal(err)
	}
	f.cancel(t, job)
	if err := store.InTenant(ctx, f.worker, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "update veho.jobs set state = $2, finished_at = now(), settled_at = now() where id = $1", job, state)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return job
}

func (f fixture) outbox(t *testing.T, hook uuid.UUID) (n int) {
	t.Helper()
	if err := f.migrator.QueryRow(context.Background(), "select count(*) from veho.webhook_outbox where webhook_id = $1", hook).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f fixture) due(t *testing.T) {
	t.Helper()
	f.sql(t, "update veho.webhook_outbox set next_attempt_at = now() where state = 'pending'")
}

// WH-T5, WH-T6, WH-T7: registration, limits, tenancy, the switch.
func TestWebhookRegistration(t *testing.T) {
	f := hookFixture(t)
	ctx := context.Background()
	w, secret, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "ci", URL: "https://hooks.example.com/veho", Events: []string{"job.succeeded", "job.failed"}})
	if err != nil || !strings.HasPrefix(secret, "whsec_") || !w.Enabled {
		t.Fatalf("create: %+v, %q, %v", w, secret, err)
	}
	// the secret is in the secret store, under the reserved name, and only there
	got, err := f.secrets.Fetch(ctx, f.tenant, secretRef(w.ID))
	if err != nil || string(got.Bytes()) != secret {
		t.Fatalf("stored secret: %v", err)
	}
	var inRow int
	if err := f.migrator.QueryRow(ctx, "select count(*) from veho.webhooks where id = $1 and (url || name || events::text) like '%' || $2 || '%'", w.ID, secret).Scan(&inRow); err != nil || inRow != 0 {
		t.Errorf("the secret is in the webhooks row: %d, %v", inRow, err)
	}
	if _, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "ci", URL: "https://hooks.example.com/x", Events: []string{"job.failed"}}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate: %v", err)
	}
	for _, bad := range []Webhook{
		{Name: "Bad Name", URL: "https://h.example.com/", Events: []string{"job.failed"}},
		{Name: "a", URL: "http://h.example.com/", Events: []string{"job.failed"}},
		{Name: "a", URL: "https://u:p@h.example.com/", Events: []string{"job.failed"}},
		{Name: "a", URL: "https://h.example.com/", Events: nil},
		{Name: "a", URL: "https://h.example.com/", Events: []string{"job.exploded"}},
	} {
		if _, _, err := f.e.CreateWebhook(ctx, f.tenant, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	// the secrets API's reserved prefix is the API's to enforce (api tests); the engine's list shows it
	if list, err := f.e.ListWebhooks(ctx, f.tenant); err != nil || len(list) != 1 {
		t.Errorf("list: %v, %v", list, err)
	}
	up, err := f.e.UpdateWebhook(ctx, f.tenant, "ci", WebhookUpdate{Events: []string{"job.cancelled"}})
	if err != nil || len(up.Events) != 1 || up.Events[0] != "job.cancelled" {
		t.Errorf("update: %+v, %v", up, err)
	}
	// the limit (WH-T6)
	for i := 1; i < MaxWebhooks; i++ {
		if _, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "w" + string(rune('a'+i)), URL: "https://h.example.com/", Events: []string{"job.failed"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "one-too-many", URL: "https://h.example.com/", Events: []string{"job.failed"}}); !errors.Is(err, ErrQuota) {
		t.Errorf("past the limit: %v", err)
	}
	// another tenant sees none of it (INV-4)
	var other uuid.UUID
	if err := f.api.QueryRow(ctx, "select veho.create_tenant($1)", "other-"+uuid.New().String()).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.e.ListWebhooks(ctx, other); len(list) != 0 {
		t.Errorf("the other tenant lists %d webhooks", len(list))
	}
	if _, err := f.e.GetWebhook(ctx, other, "ci"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get across tenants: %v", err)
	}
	if err := f.e.DeleteWebhook(ctx, other, "ci"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete across tenants: %v", err)
	}
	if _, err := f.e.RotateWebhookSecret(ctx, other, "ci"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotate across tenants: %v", err)
	}
	// delete: the webhook, its events and its secret
	if err := f.e.DeleteWebhook(ctx, f.tenant, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.secrets.Fetch(ctx, f.tenant, secretRef(w.ID)); !errors.Is(err, secrets.ErrUnavailable) {
		t.Errorf("the secret survives the webhook: %v", err)
	}
	// the switch (WH-T7)
	f.e.cfg.Notifications.Enabled = false
	defer func() { f.e.cfg.Notifications.Enabled = true }()
	if _, _, err := f.e.CreateWebhook(ctx, other, Webhook{Name: "x", URL: "https://h.example.com/", Events: []string{"job.failed"}}); !errors.Is(err, ErrDisabled) || !errors.Is(err, ErrState) {
		t.Errorf("off: %v", err)
	}
}

// WH-T3: the event exists exactly when the job settled; a rolled-back
// settle leaves none; each webhook gets the events it subscribed to.
func TestWebhookOutbox(t *testing.T) {
	f := hookFixture(t)
	ctx := context.Background()
	all, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "all", URL: "https://h.example.com/a", Events: WebhookEvents})
	if err != nil {
		t.Fatal(err)
	}
	onlyFailed, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "failed", URL: "https://h.example.com/b", Events: []string{"job.failed"}})
	if err != nil {
		t.Fatal(err)
	}
	// a rolled-back settle
	job, _, err := f.e.Submit(ctx, f.tenant, Request{SourceEndpoint: "src", SourcePath: "in/rb", Policy: []byte(twoCopies)})
	if err != nil {
		t.Fatal(err)
	}
	f.cancel(t, job)
	rollback := errors.New("rollback")
	if err := store.InTenant(ctx, f.worker, f.tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "update veho.jobs set state = 'SUCCEEDED', finished_at = now() where id = $1", job); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if f.outbox(t, all.ID) != 0 {
		t.Error("an event exists for a settle that was rolled back")
	}
	f.settle(t, "SUCCEEDED")
	f.settle(t, "FAILED")
	f.settle(t, "CANCELLED")
	if got := f.outbox(t, all.ID); got != 3 {
		t.Errorf("the all-events webhook has %d events, want 3", got)
	}
	if got := f.outbox(t, onlyFailed.ID); got != 1 {
		t.Errorf("the failed-only webhook has %d events, want 1", got)
	}
	// a disabled webhook gets nothing
	off := false
	if _, err := f.e.UpdateWebhook(ctx, f.tenant, "all", WebhookUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	f.settle(t, "FAILED")
	if got := f.outbox(t, all.ID); got != 3 {
		t.Errorf("a disabled webhook got an event: %d", got)
	}
	var typ string
	var payload []byte
	if err := f.migrator.QueryRow(ctx, "select event_type, payload from veho.webhook_outbox where webhook_id = $1 and event_type = 'job.succeeded'", all.ID).Scan(&typ, &payload); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		TenantID string `json:"tenant_id"`
		Job      struct {
			ID, State    string
			Destinations map[string]int
		} `json:"job"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil || doc.Job.State != "SUCCEEDED" || doc.TenantID != f.tenant.String() || doc.Job.Destinations == nil {
		t.Errorf("payload %s: %+v, %v", payload, doc, err)
	}
	if strings.Contains(string(payload), "in/") {
		t.Errorf("the payload names a path: %s", payload)
	}
}

// WH-T4: at least once under a receiver that fails then recovers, the same
// event id at each attempt, the signature, the backoff, and one request in
// flight per webhook.
func TestWebhookDelivery(t *testing.T) {
	f := hookFixture(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	w, secret, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "ci", URL: rcv.URL, Events: []string{"job.succeeded"}})
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, "SUCCEEDED")
	f.settle(t, "SUCCEEDED")
	rcv.answer(http.StatusInternalServerError)
	if n, err := f.e.DeliverWebhooks(ctx); err != nil || n != 1 {
		t.Fatalf("first pass: %d, %v", n, err) // one request in flight per webhook
	}
	if n, _ := f.e.DeliverWebhooks(ctx); n != 0 {
		t.Errorf("an event in backoff was tried again at once: %d", n)
	}
	rcv.answer(http.StatusOK)
	f.due(t)
	if _, err := f.e.DeliverWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	if _, err := f.e.DeliverWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	got := rcv.deliveries()
	if len(got) != 3 {
		t.Fatalf("%d deliveries, want 3 (a failure, its retry, the second event)", len(got))
	}
	if got[0].header.Get("Veho-Event-Id") != got[1].header.Get("Veho-Event-Id") || string(got[0].body) != string(got[1].body) {
		t.Error("a retry is not the same event")
	}
	if got[2].header.Get("Veho-Event-Id") == got[0].header.Get("Veho-Event-Id") {
		t.Error("two events share an id")
	}
	for _, d := range got {
		if err := webhook.Verify(d.header.Get("Veho-Signature"), d.body, []byte(secret), time.Now(), webhook.Tolerance); err != nil {
			t.Errorf("signature: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(d.body, &doc); err != nil || doc["type"] != "job.succeeded" || len(d.body) > 4096 {
			t.Errorf("body %s: %v", d.body, err)
		}
	}
	var pending int
	if err := f.migrator.QueryRow(ctx, "select count(*) from veho.webhook_outbox where webhook_id = $1 and state = 'pending'", w.ID).Scan(&pending); err != nil || pending != 0 {
		t.Errorf("%d events still pending, %v", pending, err)
	}
	// a rotation: both secrets sign for a while
	fresh, err := f.e.RotateWebhookSecret(ctx, f.tenant, "ci")
	if err != nil || fresh == secret {
		t.Fatalf("rotate: %v", err)
	}
	f.settle(t, "SUCCEEDED")
	if _, err := f.e.DeliverWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	last := rcv.deliveries()[3]
	for _, s := range []string{secret, fresh} {
		if err := webhook.Verify(last.header.Get("Veho-Signature"), last.body, []byte(s), time.Now(), webhook.Tolerance); err != nil {
			t.Errorf("after a rotation, a receiver holding %q cannot verify: %v", s[:10], err)
		}
	}
}

// WH-T4: abandonment after 12 attempts, and the webhook is disabled after 12
// consecutive failures; its audit entries; re-enabling.
func TestWebhookAbandonAndDisable(t *testing.T) {
	f := hookFixture(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	rcv.answer(http.StatusBadGateway)
	w, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "down", URL: rcv.URL, Events: []string{"job.failed"}})
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t, "FAILED")
	for i := 0; i < webhook.MaxAttempts; i++ {
		f.due(t)
		if n, err := f.e.DeliverWebhooks(ctx); err != nil || n != 1 {
			t.Fatalf("attempt %d: %d, %v", i+1, n, err)
		}
	}
	var state string
	if err := f.migrator.QueryRow(ctx, "select state from veho.webhook_outbox where webhook_id = $1", w.ID).Scan(&state); err != nil || state != "abandoned" {
		t.Errorf("the event is %q after %d attempts: %v", state, webhook.MaxAttempts, err)
	}
	got, err := f.e.GetWebhook(ctx, f.tenant, "down")
	if err != nil || got.Enabled || got.DisabledReason == "" {
		t.Errorf("the webhook after %d failures: %+v, %v", webhook.MaxAttempts, got, err)
	}
	if f.events("WEBHOOK_DELIVERY_ABANDONED") != 1 || f.events("WEBHOOK_DISABLE") != 1 {
		t.Errorf("audit: %d abandoned, %d disabled", f.events("WEBHOOK_DELIVERY_ABANDONED"), f.events("WEBHOOK_DISABLE"))
	}
	on := true
	if back, err := f.e.UpdateWebhook(ctx, f.tenant, "down", WebhookUpdate{Enabled: &on}); err != nil || !back.Enabled || back.ConsecutiveFailures != 0 || back.DisabledReason != "" {
		t.Errorf("re-enabled: %+v, %v", back, err)
	}
}

// W-1: a webhook whose URL points inside is never reached, and the tenant
// learns only a class.
func TestWebhookGuardAndTest(t *testing.T) {
	f := hookFixture(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	if _, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "ok", URL: rcv.URL, Events: []string{"job.failed"}}); err != nil {
		t.Fatal(err)
	}
	res, err := f.e.TestWebhook(ctx, f.tenant, "ok")
	if err != nil || !res.OK {
		t.Fatalf("test: %+v, %v", res, err)
	}
	if d := rcv.deliveries(); len(d) != 1 || !strings.Contains(string(d[0].body), `"type":"ping"`) {
		t.Fatalf("the ping did not arrive: %v", d)
	}
	// no allowed range: the loopback receiver is refused
	g, _ := webhook.NewGuard(nil)
	f.e.hooks = g.WithTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the test's own receiver
	res, err = f.e.TestWebhook(ctx, f.tenant, "ok")
	if err != nil || res.OK || res.Class != "forbidden" || strings.Contains(res.Message, "127.0.0.1") {
		t.Errorf("a loopback target: %+v, %v", res, err)
	}
	if len(rcv.deliveries()) != 1 {
		t.Error("the refused target was reached")
	}
	// the rate limit (W-10)
	var limited bool
	for i := 0; i < testPerMinute+2; i++ {
		if _, err := f.e.TestWebhook(ctx, f.tenant, "ok"); errors.Is(err, ErrQuota) {
			limited = true
		}
	}
	if !limited {
		t.Errorf("TestWebhook is not rate-limited")
	}
}

// §3.4: the retention, and the cap on pending events.
func TestWebhookRetention(t *testing.T) {
	f := hookFixture(t)
	ctx := context.Background()
	w, _, err := f.e.CreateWebhook(ctx, f.tenant, Webhook{Name: "r", URL: "https://h.example.com/", Events: []string{"job.failed"}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(t, `insert into veho.webhook_outbox (tenant_id, webhook_id, event_type, payload, state, finished_at)
		values ($1, $2, 'job.failed', '{}', 'delivered', now() - interval '2 days'),
		       ($1, $2, 'job.failed', '{}', 'delivered', now()),
		       ($1, $2, 'job.failed', '{}', 'abandoned', now() - interval '40 days')`, f.tenant, w.ID)
	f.sql(t, "insert into veho.webhook_outbox (tenant_id, webhook_id, event_type, payload) select $1, $2, 'job.failed', '{}' from generate_series(1, $3)", f.tenant, w.ID, webhookMaxPending+5)
	if err := f.e.purgeWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	var delivered, abandoned, pending int
	if err := f.migrator.QueryRow(ctx, `select count(*) filter (where state = 'delivered'), count(*) filter (where state = 'abandoned'), count(*) filter (where state = 'pending')
		from veho.webhook_outbox where webhook_id = $1`, w.ID).Scan(&delivered, &abandoned, &pending); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 || abandoned != 5 || pending != webhookMaxPending {
		t.Errorf("after the purge: %d delivered, %d abandoned, %d pending", delivered, abandoned, pending)
	}
}
