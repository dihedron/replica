package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/webhook"
)

// The delivery of webhooks (veho-webhooks.md §2): one dispatcher at a time
// takes the head of each webhook's queue when it is due, posts it, and
// records the outcome. At least once: a crash between the post and the
// record delivers again, with the same event id.

const (
	webhookLock = "veho_webhooks"
	// webhookInterval is how often the dispatcher looks for due events.
	webhookInterval = 2 * time.Second
	// webhookConcurrency is the deliveries in flight at once (W-5).
	webhookConcurrency = 16
	webhookBatch       = 100
	// webhookPurgeEvery is how often the retention runs.
	webhookPurgeEvery = time.Minute
	// webhookMaxPending is the pending events of one webhook (§3.4).
	webhookMaxPending = 10_000
	// webhookDisableAfter is the consecutive failures that disable a webhook.
	webhookDisableAfter = webhook.MaxAttempts
)

// RunWebhooks delivers webhooks until ctx ends; it does nothing unless
// notifications are enabled.
func (e *Engine) RunWebhooks(ctx context.Context) {
	if !e.cfg.Notifications.Enabled {
		return
	}
	t := time.NewTicker(webhookInterval)
	defer t.Stop()
	var purged time.Time
	for {
		if _, err := e.DeliverWebhooks(ctx); err != nil {
			slog.WarnContext(ctx, "delivering webhooks", "error", err)
		}
		if time.Since(purged) >= webhookPurgeEvery {
			if err := e.purgeWebhooks(ctx); err != nil {
				slog.WarnContext(ctx, "purging the webhook outbox", "error", err)
			} else {
				purged = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// DeliverWebhooks runs one pass if this node takes the lock, and returns the
// events it tried.
func (e *Engine) DeliverWebhooks(ctx context.Context) (int, error) {
	release, locked, err := store.TryLock(ctx, e.worker, webhookLock, 30*time.Second)
	if err != nil || !locked {
		return 0, err
	}
	defer release()
	type due struct {
		id, tenant, webhook uuid.UUID
		typ                 string
		payload             map[string]any
		created             time.Time
		url                 string
		prevUntil           *time.Time
		attempts            int
	}
	rows, err := e.worker.Query(ctx, "select id, tenant_id, webhook_id, event_type, payload, created_at, attempts, url, prev_secret_until from veho.due_webhook_events($1)", webhookBatch)
	if err != nil {
		return 0, fmt.Errorf("due webhook events: %w", err)
	}
	var events []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tenant, &d.webhook, &d.typ, &d.payload, &d.created, &d.attempts, &d.url, &d.prevUntil); err != nil {
			rows.Close()
			return 0, fmt.Errorf("due webhook events: %w", err)
		}
		events = append(events, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("due webhook events: %w", err)
	}
	sem := make(chan struct{}, webhookConcurrency)
	var wg sync.WaitGroup
	for _, d := range events {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			ev, err := eventOf(d.id, d.typ, d.created, d.tenant, d.payload)
			if err != nil {
				slog.ErrorContext(ctx, "cannot build a webhook event", "tenant_id", d.tenant.String(), "event_id", d.id.String(), "error", err)
				return
			}
			res := e.postWebhook(ctx, d.tenant, d.webhook, d.url, d.prevUntil, ev, d.id)
			e.recordDelivery(ctx, d.tenant, d.webhook, d.id, d.attempts, res)
		})
	}
	wg.Wait()
	return len(events), nil
}

// eventOf builds the document of §3.2 from the outbox row; it is the same
// bytes at every attempt.
func eventOf(id uuid.UUID, typ string, created time.Time, tenant uuid.UUID, payload map[string]any) (webhook.Event, error) {
	doc := map[string]any{"id": id.String(), "type": typ, "occurred_at": created.UTC().Format(time.RFC3339), "tenant_id": tenant.String()}
	for k, v := range payload {
		if _, fixed := doc[k]; !fixed {
			doc[k] = v
		}
	}
	body, err := json.Marshal(doc)
	return webhook.Event{ID: id.String(), Type: typ, OccurredAt: created, Body: body}, err
}

func (e *Engine) postWebhook(ctx context.Context, tenant, id uuid.UUID, url string, prevUntil *time.Time, ev webhook.Event, event uuid.UUID) webhook.Result {
	secs, err := e.webhookSecrets(ctx, tenant, id, prevUntil)
	if err != nil {
		slog.WarnContext(ctx, "cannot read a webhook secret", "tenant_id", tenant.String(), "webhook_id", id.String(), "event_id", event.String(), "error", err)
		return webhook.Result{Class: "secret", Message: "the secret is unavailable"}
	}
	defer zero(secs)
	return e.hooks.Deliver(ctx, url, ev, userAgent(), time.Now(), secretBytes(secs)...)
}

// recordDelivery stores an outcome, and audits what it changed (§3.1);
// attempts is the number of attempts made before this one.
func (e *Engine) recordDelivery(ctx context.Context, tenant, hook, event uuid.UUID, attempts int, res webhook.Result) {
	var state string
	var disabled bool
	if err := e.worker.QueryRow(ctx, "select state, disabled from veho.webhook_delivered($1, $2, $3, make_interval(secs => $4), $5, $6)",
		event, res.OK, res.Class, webhook.Backoff(attempts+1).Seconds(), webhook.MaxAttempts, webhookDisableAfter).Scan(&state, &disabled); err != nil {
		// not recorded: the event stays due and is delivered again (at least once)
		slog.WarnContext(ctx, "cannot record a webhook delivery", "tenant_id", tenant.String(), "webhook_id", hook.String(), "event_id", event.String(), "error", err)
		return
	}
	result := "failure"
	switch {
	case res.OK:
		result = "success"
	case state == "abandoned":
		result = "abandoned"
	}
	e.metrics.webhooks.WithLabelValues(result).Inc()
	if state == "abandoned" {
		e.audit.Emit(ctx, audit.Event{Action: "WEBHOOK_DELIVERY_ABANDONED", Outcome: audit.Failed, TenantID: tenant.String(), ResourceType: "webhook",
			ResourceID: hook.String(), Details: map[string]any{"event_id": event.String(), "class": res.Class}})
	}
	if disabled {
		e.audit.Emit(ctx, audit.Event{Action: "WEBHOOK_DISABLE", Outcome: audit.Success, TenantID: tenant.String(), ResourceType: "webhook",
			ResourceID: hook.String(), Details: map[string]any{"reason": "consecutive failed deliveries", "class": res.Class}})
	}
}

func (e *Engine) purgeWebhooks(ctx context.Context) error {
	n := e.cfg.Notifications
	var capped int
	if err := e.worker.QueryRow(ctx, "select veho.purge_webhook_outbox(make_interval(secs => $1), make_interval(secs => $2), $3)",
		n.DeliveredRetention.Seconds(), n.AbandonedRetention.Seconds(), webhookMaxPending).Scan(&capped); err != nil {
		return err
	}
	if capped > 0 {
		e.metrics.webhooks.WithLabelValues("abandoned").Add(float64(capped))
		e.audit.Emit(ctx, audit.Event{Action: "WEBHOOK_DELIVERY_ABANDONED", Outcome: audit.Success, ResourceType: "webhook",
			Details: map[string]any{"events": capped, "reason": "outbox cap"}})
	}
	return nil
}
