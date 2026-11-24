package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/ratelimit"
	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/webhook"
	"github.com/dihedron/veho/pkg/metadata"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Webhooks of a tenant (veho-webhooks.md): the registration. The delivery is
// in webhooks_dispatch.go.

// WebhookEvents are the events a webhook may subscribe to (§2).
var WebhookEvents = []string{"job.succeeded", "job.failed", "job.cancelled"}

const (
	// MaxWebhooks is the webhooks a tenant may register (§3.4).
	MaxWebhooks = 10
	// prevSecretValid is how long a rotated secret still signs (§2).
	prevSecretValid = 24 * time.Hour
	// testPerMinute is the TestWebhook rate per tenant (W-10).
	testPerMinute = 6
	// SecretPrefix is the reserved prefix of the webhooks' secrets, which the
	// secrets API refuses to a tenant.
	SecretPrefix = "webhook."
)

// ErrDisabled is a feature that is switched off (409).
var ErrDisabled = fmt.Errorf("%w: notifications are disabled on this cluster", ErrState)

// Webhook is a tenant's webhook, without its secret.
type Webhook struct {
	ID                  uuid.UUID
	Name, URL           string
	Events              []string
	Enabled             bool
	DisabledReason      string
	ConsecutiveFailures int
	CreatedAt           time.Time
	LastSuccessAt       *time.Time
	LastFailureAt       *time.Time
	PrevSecretUntil     *time.Time
}

const webhookColumns = `id, name, url, events, enabled, coalesce(disabled_reason, ''), consecutive_failures, created_at, last_success_at, last_failure_at, prev_secret_until`

func scanWebhooks(rows pgx.Rows) ([]Webhook, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (w Webhook, err error) {
		err = r.Scan(&w.ID, &w.Name, &w.URL, &w.Events, &w.Enabled, &w.DisabledReason, &w.ConsecutiveFailures, &w.CreatedAt, &w.LastSuccessAt, &w.LastFailureAt, &w.PrevSecretUntil)
		return
	})
}

func secretName(id uuid.UUID) string { return SecretPrefix + id.String() }
func secretRef(id uuid.UUID) string  { return secrets.RefPrefix + secretName(id) }
func prevRef(id uuid.UUID) string    { return secretRef(id) + ".prev" }

// newSecret is a fresh webhook secret: 256 random bits, shown once.
func newSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("webhook secret: %w", err)
	}
	return []byte("whsec_" + base64.RawURLEncoding.EncodeToString(b)), nil
}

func (e *Engine) checkWebhook(name, url string, events []string) error {
	if !validEndpointName.MatchString(name) {
		return fmt.Errorf("%w: webhook name %q: want %s", ErrInvalid, name, validEndpointName)
	}
	if err := webhook.ValidateURL(url); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return checkEvents(events)
}

func checkEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("%w: events: at least one of %s", ErrInvalid, strings.Join(WebhookEvents, ", "))
	}
	for _, ev := range events {
		if !slices.Contains(WebhookEvents, ev) {
			return fmt.Errorf("%w: event %q: want one of %s", ErrInvalid, ev, strings.Join(WebhookEvents, ", "))
		}
	}
	return nil
}

// CreateWebhook registers a webhook and returns its secret, which is shown
// once and kept only in the secret store.
func (e *Engine) CreateWebhook(ctx context.Context, tenant uuid.UUID, w Webhook) (Webhook, string, error) {
	if !e.cfg.Notifications.Enabled {
		return Webhook{}, "", ErrDisabled
	}
	if err := e.checkWebhook(w.Name, w.URL, w.Events); err != nil {
		return Webhook{}, "", fmt.Errorf("engine: create webhook: %w", err)
	}
	secret, err := newSecret()
	if err != nil {
		return Webhook{}, "", fmt.Errorf("engine: create webhook: %w", err)
	}
	w.ID = uuid.New()
	// the secret first: a webhook never exists without one
	if _, err := e.secrets.Put(ctx, tenant, secretName(w.ID), secret); err != nil {
		return Webhook{}, "", fmt.Errorf("engine: create webhook: %w", err)
	}
	var out Webhook
	err = store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "select count(*) from veho.webhooks").Scan(&n); err != nil {
			return err
		}
		if n >= MaxWebhooks {
			return fmt.Errorf("%w: a tenant has at most %d webhooks", ErrQuota, MaxWebhooks)
		}
		rows, err := tx.Query(ctx, `insert into veho.webhooks (id, tenant_id, name, url, events) values ($1, veho.current_tenant(), $2, $3, $4)
			returning `+webhookColumns, w.ID, w.Name, w.URL, w.Events)
		if err != nil {
			return err
		}
		ws, err := scanWebhooks(rows)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
			return fmt.Errorf("%w: webhook %q exists", ErrConflict, w.Name)
		}
		if err == nil {
			out = ws[0]
		}
		return err
	})
	e.emitWebhook(ctx, "WEBHOOK_CREATE", tenant, w.Name, err)
	if err != nil {
		_ = e.secrets.Delete(ctx, tenant, secretName(w.ID)) // no webhook, no secret
		return Webhook{}, "", fmt.Errorf("engine: create webhook: %w", err)
	}
	return out, string(secret), nil
}

// GetWebhook returns a webhook.
func (e *Engine) GetWebhook(ctx context.Context, tenant uuid.UUID, name string) (Webhook, error) {
	var out Webhook
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+webhookColumns+" from veho.webhooks where name = $1", name)
		if err != nil {
			return err
		}
		ws, err := scanWebhooks(rows)
		if err != nil {
			return err
		}
		if len(ws) == 0 {
			return fmt.Errorf("webhook %q: %w", name, ErrNotFound)
		}
		out = ws[0]
		return nil
	})
	if err != nil {
		return Webhook{}, fmt.Errorf("engine: get webhook: %w", err)
	}
	return out, nil
}

// ListWebhooks returns the tenant's webhooks, by name.
func (e *Engine) ListWebhooks(ctx context.Context, tenant uuid.UUID) ([]Webhook, error) {
	var out []Webhook
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+webhookColumns+" from veho.webhooks order by name")
		if err != nil {
			return err
		}
		out, err = scanWebhooks(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("engine: list webhooks: %w", err)
	}
	return out, nil
}

// WebhookUpdate is what UpdateWebhook may change; nil leaves a field.
type WebhookUpdate struct {
	Events  []string
	Enabled *bool
}

// UpdateWebhook changes a webhook's events or switches it on or off;
// enabling clears the failures that disabled it.
func (e *Engine) UpdateWebhook(ctx context.Context, tenant uuid.UUID, name string, u WebhookUpdate) (Webhook, error) {
	if u.Events != nil {
		if err := checkEvents(u.Events); err != nil {
			return Webhook{}, fmt.Errorf("engine: update webhook: %w", err)
		}
	}
	var out Webhook
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `update veho.webhooks set
			events = coalesce($2, events),
			enabled = coalesce($3, enabled),
			disabled_reason = case when $3 is null then disabled_reason when $3 then null else coalesce(disabled_reason, 'disabled by the tenant') end,
			consecutive_failures = case when $3 then 0 else consecutive_failures end
			where name = $1 returning `+webhookColumns, name, u.Events, u.Enabled)
		if err != nil {
			return err
		}
		ws, err := scanWebhooks(rows)
		if err != nil {
			return err
		}
		if len(ws) == 0 {
			return fmt.Errorf("webhook %q: %w", name, ErrNotFound)
		}
		out = ws[0]
		return nil
	})
	e.emitWebhook(ctx, "WEBHOOK_UPDATE", tenant, name, err)
	if err != nil {
		return Webhook{}, fmt.Errorf("engine: update webhook: %w", err)
	}
	return out, nil
}

// RotateWebhookSecret gives the webhook a new secret, shown once; the old
// one keeps signing for 24 hours, next to the new one (§2).
func (e *Engine) RotateWebhookSecret(ctx context.Context, tenant uuid.UUID, name string) (string, error) {
	w, err := e.GetWebhook(ctx, tenant, name)
	if err != nil {
		return "", fmt.Errorf("engine: rotate webhook: %w", err)
	}
	fresh, err := newSecret()
	if err != nil {
		return "", fmt.Errorf("engine: rotate webhook: %w", err)
	}
	err = func() error {
		cur, err := e.secrets.Fetch(ctx, tenant, secretRef(w.ID))
		if err != nil {
			return err
		}
		defer cur.Zero()
		if _, err := e.secrets.Put(ctx, tenant, secretName(w.ID)+".prev", cur.Bytes()); err != nil {
			return err
		}
		if _, err := e.secrets.Put(ctx, tenant, secretName(w.ID), fresh); err != nil {
			return err
		}
		return store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "update veho.webhooks set prev_secret_until = now() + $2::interval where id = $1", w.ID, prevSecretValid.String())
			return err
		})
	}()
	e.emitWebhook(ctx, "WEBHOOK_ROTATE", tenant, name, err)
	if err != nil {
		return "", fmt.Errorf("engine: rotate webhook: %w", err)
	}
	return string(fresh), nil
}

// DeleteWebhook removes a webhook, its pending events and its secrets.
func (e *Engine) DeleteWebhook(ctx context.Context, tenant uuid.UUID, name string) error {
	var id uuid.UUID
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "delete from veho.webhooks where name = $1 returning id", name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("webhook %q: %w", name, ErrNotFound)
		}
		return err
	})
	e.emitWebhook(ctx, "WEBHOOK_DELETE", tenant, name, err)
	if err != nil {
		return fmt.Errorf("engine: delete webhook: %w", err)
	}
	for _, n := range []string{secretName(id), secretName(id) + ".prev"} {
		if err := e.secrets.Delete(ctx, tenant, n); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			slog.WarnContext(ctx, "cannot delete a webhook secret", "tenant_id", tenant.String(), "webhook", name, "error", err)
		}
	}
	return nil
}

// testLimiter limits TestWebhook per tenant (W-10), per node.
var testLimiter ratelimit.Limiter

// TestWebhook posts a signed ping to the webhook now and returns what
// happened, as a tenant may see it (W-2); nothing is stored. The ping is
// rate-limited, and the same guard as a delivery applies.
func (e *Engine) TestWebhook(ctx context.Context, tenant uuid.UUID, name string) (webhook.Result, error) {
	if !e.cfg.Notifications.Enabled {
		return webhook.Result{}, ErrDisabled
	}
	if !testLimiter.Allow(tenant.String(), ratelimit.Rate{PerSecond: float64(testPerMinute) / 60, Burst: testPerMinute}) {
		return webhook.Result{}, fmt.Errorf("engine: test webhook: %w: at most %d a minute", ErrQuota, testPerMinute)
	}
	w, err := e.GetWebhook(ctx, tenant, name)
	if err != nil {
		return webhook.Result{}, fmt.Errorf("engine: test webhook: %w", err)
	}
	secs, err := e.webhookSecrets(ctx, tenant, w.ID, w.PrevSecretUntil)
	if err != nil {
		return webhook.Result{}, fmt.Errorf("engine: test webhook: %w", err)
	}
	defer zero(secs)
	now := time.Now()
	id := uuid.New().String()
	body, err := json.Marshal(map[string]any{"id": id, "type": "ping", "occurred_at": now.UTC().Format(time.RFC3339), "tenant_id": tenant.String()})
	if err != nil {
		return webhook.Result{}, fmt.Errorf("engine: test webhook: %w", err)
	}
	res := e.hooks.Deliver(ctx, w.URL, webhook.Event{ID: id, Type: "ping", OccurredAt: now, Body: body}, userAgent(), now, secretBytes(secs)...)
	e.audit.Emit(ctx, audit.Event{Action: "WEBHOOK_TEST", Outcome: outcome(res.OK), TenantID: tenant.String(), ResourceType: "webhook", ResourceName: name,
		Details: map[string]any{"class": res.Class, "status": res.Status}})
	return res, nil
}

func userAgent() string { return "veho/" + metadata.Version }

func outcome(ok bool) string {
	if ok {
		return audit.Success
	}
	return audit.Failed
}

// webhookSecrets are the secrets a delivery signs with: the current one and,
// while prevUntil is in the future, the one a rotation replaced.
func (e *Engine) webhookSecrets(ctx context.Context, tenant, id uuid.UUID, prevUntil *time.Time) ([]*secrets.Secret, error) {
	cur, err := e.secrets.Fetch(ctx, tenant, secretRef(id))
	if err != nil {
		return nil, err
	}
	out := []*secrets.Secret{cur}
	if prevUntil != nil && prevUntil.After(time.Now()) {
		if prev, err := e.secrets.Fetch(ctx, tenant, prevRef(id)); err == nil {
			out = append(out, prev)
		}
	}
	return out, nil
}

func zero(ss []*secrets.Secret) {
	for _, s := range ss {
		s.Zero()
	}
}

func secretBytes(ss []*secrets.Secret) [][]byte {
	out := make([][]byte, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Bytes())
	}
	return out
}

func (e *Engine) emitWebhook(ctx context.Context, action string, tenant uuid.UUID, name string, err error) {
	ev := audit.Event{Action: action, Outcome: audit.Success, TenantID: tenant.String(), ResourceType: "webhook", ResourceName: name}
	if err != nil {
		ev.Outcome, ev.Details = audit.Failed, map[string]string{"error": err.Error()}
	}
	e.audit.Emit(ctx, ev)
}
