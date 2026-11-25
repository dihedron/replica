package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/flowcheck"
	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/pkg/plugin"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// The network checks of the endpoints (.specs/veho-network-checks.md).
const (
	checkWindow = 2 * time.Minute // a check not complete by then is expired
	keptChecks  = 10              // per endpoint
)

// The states of a check.
const (
	CheckRunning  = "running"
	CheckComplete = "complete"
	CheckExpired  = "expired"
)

// EndpointCheck is a check of an endpoint's flows from every node.
type EndpointCheck struct {
	ID       uuid.UUID
	Endpoint string
	Created  time.Time
	Flows    []plugin.Flow
	Error    string // why the flows could not be found
	State    string
	Nodes    []NodeCheck
}

// NodeCheck is one node's answer to a check.
type NodeCheck struct {
	Node, Region string
	Answered     bool
	Checked      time.Time
	Results      []flowcheck.Result
}

// Unreachable counts the flows some node could not reach.
func (c EndpointCheck) Unreachable() int {
	n := 0
	for _, nc := range c.Nodes {
		for _, r := range nc.Results {
			if r.Status == flowcheck.Unreachable {
				n++
			}
		}
	}
	return n
}

// CheckEndpoint starts a check of the endpoint's flows, or returns the one
// in flight (NC-3): the flows are found here, with the credentials, and
// every executor alive answers at its heartbeat.
func (e *Engine) CheckEndpoint(ctx context.Context, tenant uuid.UUID, name string) (EndpointCheck, error) {
	var id uuid.UUID
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var endpoint uuid.UUID
		if err := tx.QueryRow(ctx, "select id from veho.endpoints where name = $1", name).Scan(&endpoint); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("endpoint %q: %w", name, ErrNotFound)
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('veho-check:' || $1::text))", endpoint); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `select c.id from veho.endpoint_checks c where c.endpoint_id = $1 and c.created_at > now() - $2::interval
			and (select count(*) from veho.endpoint_check_results r where r.check_id = c.id) < cardinality(c.nodes)
			order by c.created_at desc limit 1`, endpoint, checkWindow.String()).Scan(&id)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err // one in flight, or a failure
		}
		flows, ferr := e.flows(ctx, tenant, endpoint)
		var reason *string
		if ferr != nil {
			s := ferr.Error()
			reason, flows = &s, nil
		}
		b, _ := json.Marshal(flows) // cannot fail
		if err := tx.QueryRow(ctx, `insert into veho.endpoint_checks (tenant_id, endpoint_id, flows, error, nodes)
			values (veho.current_tenant(), $1, $2, $3,
				array(select x.id from veho.executors x where x.last_seen > now() - $4::interval order by x.id))
			returning id`, endpoint, b, reason, e.cfg.Engine.LeaseTTL.String()).Scan(&id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `delete from veho.endpoint_checks where endpoint_id = $1 and id not in
			(select id from veho.endpoint_checks where endpoint_id = $1 order by created_at desc limit $2)`, endpoint, keptChecks)
		return err
	})
	if err != nil {
		return EndpointCheck{}, fmt.Errorf("engine: check endpoint: %w", err)
	}
	c, err := e.GetEndpointCheck(ctx, tenant, name, id)
	if err == nil {
		// counts, not addresses (NC-5)
		e.audit.Emit(ctx, audit.Event{Action: "ENDPOINT_CHECKED", Outcome: audit.Success, TenantID: tenant.String(),
			ResourceType: "endpoint", ResourceName: name,
			Details: map[string]string{"check": id.String(), "flows": fmt.Sprint(len(c.Flows)), "nodes": fmt.Sprint(len(c.Nodes))}})
	}
	return c, err
}

// checkLater checks an endpoint just created or updated, without making
// the caller wait (decision 4).
func (e *Engine) checkLater(tenant uuid.UUID, name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := e.CheckEndpoint(ctx, tenant, name); err != nil {
			slog.WarnContext(ctx, "the check of a new endpoint", "tenant_id", tenant, "endpoint", name, "error", err)
		}
	}()
}

// flows are the endpoint's, from its driver, at most plugin.MaxFlows.
func (e *Engine) flows(ctx context.Context, tenant, endpoint uuid.UUID) ([]plugin.Flow, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, c, err := e.open(ctx, tenant, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	l, ok := c.(plugin.FlowLister)
	if !ok {
		return nil, nil
	}
	flows, err := l.Flows(ctx)
	switch {
	case err != nil:
		return nil, fmt.Errorf("the driver's flows: %w", err)
	case len(flows) > plugin.MaxFlows:
		return nil, fmt.Errorf("the driver lists %d flows, more than %d", len(flows), plugin.MaxFlows)
	}
	return flows, nil
}

// GetEndpointCheck reads a check of the endpoint.
func (e *Engine) GetEndpointCheck(ctx context.Context, tenant uuid.UUID, name string, id uuid.UUID) (EndpointCheck, error) {
	cs, err := e.endpointChecks(ctx, tenant, name, &id)
	if err != nil {
		return EndpointCheck{}, err
	}
	if len(cs) == 0 {
		return EndpointCheck{}, fmt.Errorf("engine: check %s of endpoint %q: %w", id, name, ErrNotFound)
	}
	return cs[0], nil
}

// ListEndpointChecks reads the endpoint's last checks, newest first.
func (e *Engine) ListEndpointChecks(ctx context.Context, tenant uuid.UUID, name string) ([]EndpointCheck, error) {
	return e.endpointChecks(ctx, tenant, name, nil)
}

func (e *Engine) endpointChecks(ctx context.Context, tenant uuid.UUID, name string, id *uuid.UUID) ([]EndpointCheck, error) {
	var out []EndpointCheck
	err := store.InTenant(ctx, e.api, tenant, func(tx pgx.Tx) error {
		var endpoint uuid.UUID
		if err := tx.QueryRow(ctx, "select id from veho.endpoints where name = $1", name).Scan(&endpoint); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("endpoint %q: %w", name, ErrNotFound)
		} else if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select c.id, c.created_at, c.flows, coalesce(c.error, ''), c.nodes, c.created_at < now() - $3::interval,
				coalesce((select json_agg(json_build_object('node', r.node_id, 'region', r.region, 'checked', r.checked_at, 'results', r.results))
					from veho.endpoint_check_results r where r.check_id = c.id), '[]')
			from veho.endpoint_checks c where c.endpoint_id = $1 and ($2::uuid is null or c.id = $2)
			order by c.created_at desc`, endpoint, id, checkWindow.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := EndpointCheck{Endpoint: name}
			var flows, answers []byte
			var nodes []string
			var old bool
			if err := rows.Scan(&c.ID, &c.Created, &flows, &c.Error, &nodes, &old, &answers); err != nil {
				return err
			}
			if err := c.fill(flows, answers, nodes, old); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("engine: endpoint checks: %w", err)
	}
	return out, nil
}

// fill decodes a check's flows and answers, and gives its state.
func (c *EndpointCheck) fill(flows, answers []byte, nodes []string, old bool) error {
	if err := json.Unmarshal(flows, &c.Flows); err != nil {
		return err
	}
	var as []struct {
		Node    string             `json:"node"`
		Region  string             `json:"region"`
		Checked time.Time          `json:"checked"`
		Results []flowcheck.Result `json:"results"`
	}
	if err := json.Unmarshal(answers, &as); err != nil {
		return err
	}
	by := map[string]NodeCheck{}
	for _, a := range as {
		by[a.Node] = NodeCheck{Node: a.Node, Region: a.Region, Answered: true, Checked: a.Checked, Results: a.Results}
	}
	for _, n := range nodes {
		nc, ok := by[n]
		if !ok {
			nc = NodeCheck{Node: n}
		}
		c.Nodes = append(c.Nodes, nc)
	}
	switch {
	case len(as) >= len(nodes):
		c.State = CheckComplete
	case old:
		c.State = CheckExpired
	default:
		c.State = CheckRunning
	}
	return nil
}

// RunEndpointChecks answers this node's checks at every heartbeat until
// ctx ends.
func (e *Engine) RunEndpointChecks(ctx context.Context) {
	t := time.NewTicker(e.cfg.Engine.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.answerChecks(ctx); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "endpoint checks", "error", err)
			}
		}
	}
}

// answerChecks probes the flows of the checks this node has to answer and
// records its results.
func (e *Engine) answerChecks(ctx context.Context) error {
	rows, err := e.worker.Query(ctx, "select tenant_id, check_id from veho.pending_endpoint_checks($1, $2::interval)", e.executor, checkWindow.String())
	if err != nil {
		return fmt.Errorf("engine: pending checks: %w", err)
	}
	type pending struct{ Tenant, Check uuid.UUID }
	ps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
	if err != nil {
		return fmt.Errorf("engine: pending checks: %w", err)
	}
	for _, p := range ps {
		if err := e.answerCheck(ctx, p.Tenant, p.Check); err != nil {
			slog.ErrorContext(ctx, "endpoint check", "tenant_id", p.Tenant, "check_id", p.Check, "error", err)
		}
	}
	return nil
}

func (e *Engine) answerCheck(ctx context.Context, tenant, check uuid.UUID) error {
	var flowsJSON []byte
	var driver string
	if err := store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select c.flows, e.driver from veho.endpoint_checks c join veho.endpoints e on e.id = c.endpoint_id
			where c.id = $1`, check).Scan(&flowsJSON, &driver)
	}); err != nil {
		return err
	}
	var flows []plugin.Flow
	if err := json.Unmarshal(flowsJSON, &flows); err != nil {
		return err
	}
	guard, err := e.dialGuard(driver)
	if err != nil {
		return err
	}
	results := flowcheck.Check(ctx, flows, flowcheck.Options{Guard: guard})
	if results == nil {
		results = []flowcheck.Result{}
	}
	b, _ := json.Marshal(results) // cannot fail
	return store.InTenant(ctx, e.worker, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into veho.endpoint_check_results (check_id, tenant_id, node_id, region, results)
			values ($1, veho.current_tenant(), $2, $3, $4) on conflict do nothing`, check, e.executor, e.region, b)
		return err
	})
}

// dialGuard is the dial guard of a driver: its allow_cidrs setting (NC-1).
func (e *Engine) dialGuard(driver string) (*netguard.Guard, error) {
	var allow []string
	for _, d := range e.cfg.Drivers {
		if d.Name != driver {
			continue
		}
		if v, ok := d.Settings["allow_cidrs"].([]any); ok {
			for _, x := range v {
				if s, ok := x.(string); ok {
					allow = append(allow, s)
				}
			}
		}
	}
	g, err := netguard.New(allow)
	if err != nil {
		return nil, fmt.Errorf("engine: driver %s: %w", driver, err)
	}
	return g, nil
}
