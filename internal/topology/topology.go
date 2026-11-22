// Package topology models where destinations are: a tree of locations with
// named levels (for example region and az), independent of the control-plane
// sites (.specs/veho-distribution-policy.md §2).
package topology

import (
	"errors"
	"fmt"
)

// Location is a node of the tree. Parent is empty for a location of the
// first level.
type Location struct {
	ID     string
	Parent string
	Level  string
}

// Position is the location of something at every level of the tree down to
// its own: for an az it holds the region and the az; for a region only the
// region.
type Position map[string]string

// Topology is a validated tree of locations.
type Topology struct {
	levels []string
	rank   map[string]int
	loc    map[string]Location
}

// New validates the levels (ordered from the coarsest) and the locations and
// returns the topology. A location may be declared before its parent.
func New(levels []string, locations []Location) (*Topology, error) {
	if len(levels) == 0 {
		return nil, errors.New("topology: at least one level is required")
	}
	t := &Topology{levels: append([]string(nil), levels...), rank: map[string]int{}, loc: map[string]Location{}}
	for i, l := range levels {
		if l == "" {
			return nil, errors.New("topology: empty level name")
		}
		if _, dup := t.rank[l]; dup {
			return nil, fmt.Errorf("topology: duplicate level %q", l)
		}
		t.rank[l] = i
	}
	for _, l := range locations {
		if l.ID == "" {
			return nil, errors.New("topology: empty location id")
		}
		if _, dup := t.loc[l.ID]; dup {
			return nil, fmt.Errorf("topology: duplicate location %q", l.ID)
		}
		if _, ok := t.rank[l.Level]; !ok {
			return nil, fmt.Errorf("topology: location %q has unknown level %q", l.ID, l.Level)
		}
		t.loc[l.ID] = l
	}
	for _, l := range t.loc {
		r := t.rank[l.Level]
		switch {
		case r == 0 && l.Parent != "":
			return nil, fmt.Errorf("topology: location %q is at the top level and cannot have a parent", l.ID)
		case r > 0:
			p, ok := t.loc[l.Parent]
			if !ok {
				return nil, fmt.Errorf("topology: location %q has unknown parent %q", l.ID, l.Parent)
			}
			if t.rank[p.Level] != r-1 {
				return nil, fmt.Errorf("topology: parent %q of %q is not at the level above %q", p.ID, l.ID, l.Level)
			}
		}
	}
	return t, nil
}

// Levels returns the levels, coarsest first.
func (t *Topology) Levels() []string { return append([]string(nil), t.levels...) }

// HasLevel reports whether the level exists.
func (t *Topology) HasLevel(level string) bool { _, ok := t.rank[level]; return ok }

// Position returns the position of a location.
func (t *Topology) Position(id string) (Position, error) {
	l, ok := t.loc[id]
	if !ok {
		return nil, fmt.Errorf("topology: unknown location %q", id)
	}
	p := Position{}
	for {
		p[l.Level] = l.ID
		if l.Parent == "" {
			return p, nil
		}
		l = t.loc[l.Parent] // New guarantees the parent exists
	}
}
