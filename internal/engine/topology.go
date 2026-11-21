package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/dihedron/veho/internal/topology"
	"github.com/jackc/pgx/v5"
)

// Querier is a pool, a connection or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadTopology reads veho.locations; the levels are ordered by their depth
// in the tree, from the coarsest.
func LoadTopology(ctx context.Context, q Querier) (*topology.Topology, error) {
	rows, err := q.Query(ctx, "select id, coalesce(parent_id, ''), level from veho.locations")
	if err != nil {
		return nil, fmt.Errorf("engine: topology: %w", err)
	}
	locs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[topology.Location])
	if err != nil {
		return nil, fmt.Errorf("engine: topology: %w", err)
	}
	return topology.New(levels(locs), locs)
}

// levels orders the levels of locs by depth; a location whose parent is
// missing or in a cycle is left to topology.New to refuse.
func levels(locs []topology.Location) []string {
	parent := map[string]string{}
	for _, l := range locs {
		parent[l.ID] = l.Parent
	}
	depth := map[string]int{}
	for _, l := range locs {
		d := 0
		for p := l.Parent; p != "" && d <= len(locs); p = parent[p] {
			d++
		}
		if old, ok := depth[l.Level]; !ok || d < old {
			depth[l.Level] = d
		}
	}
	out := make([]string, 0, len(depth))
	for l := range depth {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(depth[a]-depth[b], strings.Compare(a, b)) })
	return out
}
