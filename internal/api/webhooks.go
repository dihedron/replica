package api

import (
	"context"
	"strings"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/engine"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The webhook service (veho-webhooks.md §3.1); its methods need the admin
// role (see the interceptors' table), and act on the caller's tenant.

type webhooks struct {
	vehov1.UnimplementedWebhookServiceServer
	s *Server
}

func optTime(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func toWebhook(w engine.Webhook) *vehov1.Webhook {
	return &vehov1.Webhook{Name: w.Name, Url: w.URL, Events: w.Events, Enabled: w.Enabled, DisabledReason: w.DisabledReason,
		ConsecutiveFailures: int32(w.ConsecutiveFailures), //nolint:gosec // at most the attempts limit
		CreatedAt:           timestamppb.New(w.CreatedAt), LastSuccessAt: optTime(w.LastSuccessAt), LastFailureAt: optTime(w.LastFailureAt)}
}

// CreateWebhook implements WebhookService.
func (ws webhooks) CreateWebhook(ctx context.Context, r *vehov1.CreateWebhookRequest) (*vehov1.CreatedWebhook, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	w, secret, err := ws.s.engine.CreateWebhook(ctx, t, engine.Webhook{Name: r.GetName(), URL: r.GetUrl(), Events: r.GetEvents()})
	if err != nil {
		return nil, err
	}
	return &vehov1.CreatedWebhook{Webhook: toWebhook(w), Secret: secret}, nil
}

// GetWebhook implements WebhookService.
func (ws webhooks) GetWebhook(ctx context.Context, r *vehov1.WebhookName) (*vehov1.Webhook, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	w, err := ws.s.engine.GetWebhook(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	return toWebhook(w), nil
}

// ListWebhooks implements WebhookService.
func (ws webhooks) ListWebhooks(ctx context.Context, _ *vehov1.ListWebhooksRequest) (*vehov1.ListWebhooksResponse, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	all, err := ws.s.engine.ListWebhooks(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &vehov1.ListWebhooksResponse{}
	for _, w := range all {
		out.Webhooks = append(out.Webhooks, toWebhook(w))
	}
	return out, nil
}

// UpdateWebhook implements WebhookService.
func (ws webhooks) UpdateWebhook(ctx context.Context, r *vehov1.UpdateWebhookRequest) (*vehov1.Webhook, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	w, err := ws.s.engine.UpdateWebhook(ctx, t, r.GetName(), engine.WebhookUpdate{Events: r.GetEvents(), Enabled: r.Enabled})
	if err != nil {
		return nil, err
	}
	return toWebhook(w), nil
}

// RotateWebhookSecret implements WebhookService.
func (ws webhooks) RotateWebhookSecret(ctx context.Context, r *vehov1.WebhookName) (*vehov1.WebhookSecret, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	secret, err := ws.s.engine.RotateWebhookSecret(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	return &vehov1.WebhookSecret{Secret: secret}, nil
}

// DeleteWebhook implements WebhookService.
func (ws webhooks) DeleteWebhook(ctx context.Context, r *vehov1.WebhookName) (*vehov1.Empty, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	return &vehov1.Empty{}, ws.s.engine.DeleteWebhook(ctx, t, r.GetName())
}

// TestWebhook implements WebhookService.
func (ws webhooks) TestWebhook(ctx context.Context, r *vehov1.WebhookName) (*vehov1.WebhookTest, error) {
	t, err := tenant(ctx)
	if err != nil {
		return nil, err
	}
	res, err := ws.s.engine.TestWebhook(ctx, t, r.GetName())
	if err != nil {
		return nil, err
	}
	return &vehov1.WebhookTest{Ok: res.OK, Status: int32(res.Status), Class: res.Class}, nil //nolint:gosec // an HTTP status
}

// reserved reports whether name is one of veho's own secrets, which a
// tenant neither reads, writes nor lists (the webhooks' secrets).
func reserved(name string) bool { return strings.HasPrefix(name, engine.SecretPrefix) }
