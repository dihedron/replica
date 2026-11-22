package policy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dihedron/veho/internal/topology"
)

func testTopology(t testing.TB) *topology.Topology {
	t.Helper()
	topo, err := topology.New([]string{"region", "az"}, []topology.Location{
		{ID: "A", Level: "region"}, {ID: "B", Level: "region"}, {ID: "C", Level: "region"},
		{ID: "a1", Parent: "A", Level: "az"}, {ID: "a2", Parent: "A", Level: "az"},
		{ID: "b1", Parent: "B", Level: "az"}, {ID: "c1", Parent: "C", Level: "az"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func testEnv(t testing.TB) Env {
	topo := testTopology(t)
	where := map[string]string{"a1-arch": "a1", "a2-arch": "a2", "b1-arch": "b1", "c1-arch": "c1", "b-region": "B"}
	return Env{
		Topology: topo,
		Resolve: func(endpoint string) (topology.Position, bool) {
			loc, ok := where[endpoint]
			if !ok {
				return nil, false
			}
			p, err := topo.Position(loc)
			return p, err == nil
		},
		Source:          Ref{Endpoint: "src", Path: "big.tar"},
		MaxDestinations: 16,
	}
}

func dest(endpoints ...string) []Destination {
	var d []Destination
	for _, e := range endpoints {
		d = append(d, Destination{Endpoint: e, Path: "replicas/big.tar"})
	}
	return d
}

func TestParseDefaults(t *testing.T) {
	p, err := Parse([]byte(`{"destinations":[{"endpoint":"a1-arch","path":"x"},{"endpoint":"b1-arch","path":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Completion.MinCopies != 2 || p.OnSatisfied != OnSatisfiedComplete || p.Verify != "sha256" ||
		p.Overwrite != "fail" || p.Deadline != Duration(168*time.Hour) ||
		p.Retry.Initial != Duration(2*time.Second) || p.Retry.MaxInterval != Duration(15*time.Minute) {
		t.Errorf("defaults not applied: %+v", p)
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":   `{"destinations":[],"bogus":1}`,
		"trailing data":   `{"destinations":[]} {}`,
		"not an object":   `[]`,
		"bad duration":    `{"deadline":"soon"}`,
		"empty":           ``,
		"negative copies": `{"completion":{"min_copies":-1}}`,
	} {
		p, err := Parse([]byte(in))
		if name == "negative copies" {
			if err == nil {
				err = p.Validate(testEnv(t)) // parsed, refused by Validate
			}
		}
		if err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestValidate(t *testing.T) {
	env := testEnv(t)
	base := func() Policy {
		return Policy{Destinations: dest("a1-arch", "a2-arch", "b1-arch", "c1-arch")}.Normalize()
	}
	tests := []struct {
		name string
		mod  func(p *Policy)
		env  func(e *Env)
		code string // "" means valid
	}{
		{"valid, all of 4", func(p *Policy) {}, nil, ""},
		{"valid, 3 of 4 over 3 regions and 3 azs", func(p *Policy) {
			p.Completion = Completion{MinCopies: 3, Spread: []Statement{{"region", 3}, {"az", 3}}}
		}, nil, ""},
		{"one destination (FR-4: N >= 1)", func(p *Policy) { p.Destinations, p.Completion = dest("a1-arch"), Completion{}; *p = p.Normalize() }, nil, ""},
		{"no destination", func(p *Policy) { p.Destinations = nil; *p = p.Normalize() }, nil, CodeInvalidPolicy},
		{"too many", func(p *Policy) {}, func(e *Env) { e.MaxDestinations = 3 }, CodeTooManyDestinations},
		{"unknown endpoint", func(p *Policy) { p.Destinations[0].Endpoint = "nope" }, nil, CodeUnknownEndpoint},
		{"empty path", func(p *Policy) { p.Destinations[0].Path = "" }, nil, CodeInvalidPolicy},
		{"duplicate", func(p *Policy) { p.Destinations[1] = p.Destinations[0] }, nil, CodeDuplicateDestination},
		{"destination is the source", func(p *Policy) {}, func(e *Env) { e.Source = Ref{Endpoint: "b1-arch", Path: "replicas/big.tar"} }, CodeDestinationIsSource},
		{"same endpoint, other path is fine", func(p *Policy) { p.Destinations[1] = Destination{Endpoint: "a1-arch", Path: "other"} }, nil, ""},
		{"min_copies above N", func(p *Policy) { p.Completion.MinCopies = 5 }, nil, CodeInvalidPolicy},
		{"negative min_copies", func(p *Policy) { p.Completion.MinCopies = -1 }, nil, CodeInvalidPolicy},
		{"unknown level", func(p *Policy) { p.Completion.Spread = []Statement{{"rack", 1}} }, nil, CodeInvalidPolicy},
		{"min_distinct 0", func(p *Policy) { p.Completion.Spread = []Statement{{"region", 0}} }, nil, CodeInvalidPolicy},
		{"duplicate level statement", func(p *Policy) { p.Completion.Spread = []Statement{{"region", 1}, {"region", 2}} }, nil, CodeInvalidPolicy},
		{"more regions than the destinations span", func(p *Policy) { p.Completion.Spread = []Statement{{"region", 4}} }, nil, CodeUnsatisfiable},
		{"more distinct locations than min_copies", func(p *Policy) {
			p.Completion = Completion{MinCopies: 2, Spread: []Statement{{"region", 3}}}
		}, nil, CodeUnsatisfiable},
		{"endpoint at region level lacks the az", func(p *Policy) {
			p.Destinations = dest("b-region", "a1-arch")
			p.Completion = Completion{MinCopies: 2, Spread: []Statement{{"az", 2}}}
		}, nil, CodeUnsatisfiable},
		{"bad on_satisfied", func(p *Policy) { p.OnSatisfied = "later" }, nil, CodeInvalidPolicy},
		{"bad overwrite", func(p *Policy) { p.Overwrite = "maybe" }, nil, CodeInvalidPolicy},
		{"bad verify", func(p *Policy) { p.Verify = "md5" }, nil, CodeInvalidPolicy},
		{"deadline not positive", func(p *Policy) { p.Deadline = -1 }, nil, CodeInvalidPolicy},
		{"retry initial above max", func(p *Policy) { p.Retry = Retry{Initial: Duration(time.Hour), MaxInterval: Duration(time.Minute)} }, nil, CodeInvalidPolicy},
		// FR-12: the tenant's bounds on the backoff
		{"retry within the tenant's bounds", func(p *Policy) { p.Retry = Retry{Initial: Duration(time.Second), MaxInterval: Duration(time.Hour)} },
			func(e *Env) { e.MinRetryInitial, e.MaxRetryInterval = time.Second, time.Hour }, ""},
		{"retry initial below the tenant's minimum", func(p *Policy) { p.Retry.Initial = Duration(100 * time.Millisecond) },
			func(e *Env) { e.MinRetryInitial = time.Second }, CodeInvalidPolicy},
		{"retry max_interval above the tenant's maximum", func(p *Policy) { p.Retry.MaxInterval = Duration(2 * time.Hour) },
			func(e *Env) { e.MaxRetryInterval = time.Hour }, CodeInvalidPolicy},
		{"no bounds, any positive retry", func(p *Policy) {
			p.Retry = Retry{Initial: Duration(time.Millisecond), MaxInterval: Duration(48 * time.Hour)}
		}, nil, ""},
	}
	for _, tc := range tests {
		p := base()
		tc.mod(&p)
		e := env
		if tc.env != nil {
			tc.env(&e)
		}
		err := p.Validate(e)
		var ve *Error
		switch {
		case tc.code == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.code != "" && !errors.As(err, &ve):
			t.Errorf("%s: want code %s, got %v", tc.name, tc.code, err)
		case tc.code != "" && ve.Code != tc.code:
			t.Errorf("%s: want code %s, got %s (%s)", tc.name, tc.code, ve.Code, ve.Message)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"destinations":[{"endpoint":"a1-arch","path":"x"},{"endpoint":"b1-arch","path":"x"}],"completion":{"min_copies":1,"spread":[{"level":"region","min_distinct":2}]},"deadline":"1h"}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(data)
		if err != nil {
			return
		}
		env := testEnv(t)
		_ = p.Validate(env) // must not panic
		// a parsed policy survives a round trip through JSON
		out, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		q, err := Parse(out)
		if err != nil || !reflect.DeepEqual(p, q) {
			t.Fatalf("round trip: %v\n%+v\n%+v", err, p, q)
		}
		if strings.Contains(string(out), "\x00") {
			t.Fatal("unexpected NUL in output")
		}
	})
}
