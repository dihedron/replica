package topology

import (
	"reflect"
	"testing"
)

func locs() []Location {
	return []Location{
		{ID: "eu-1a", Parent: "eu-1", Level: "az"}, // child declared before its parent
		{ID: "eu-1", Level: "region"},
		{ID: "eu-1b", Parent: "eu-1", Level: "az"},
		{ID: "eu-2", Level: "region"},
	}
}

func TestPosition(t *testing.T) {
	topo, err := New([]string{"region", "az"}, locs())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id   string
		want Position
	}{
		{"eu-1a", Position{"region": "eu-1", "az": "eu-1a"}},
		{"eu-2", Position{"region": "eu-2"}}, // an endpoint registered at region level has no az
	}
	for _, tc := range tests {
		got, err := topo.Position(tc.id)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Position(%q) = %v, %v; want %v", tc.id, got, err, tc.want)
		}
	}
	if _, err := topo.Position("nowhere"); err == nil {
		t.Error("Position of an unknown location must fail")
	}
	if !topo.HasLevel("az") || topo.HasLevel("rack") {
		t.Error("HasLevel is wrong")
	}
}

func TestNewRejects(t *testing.T) {
	tests := map[string]struct {
		levels []string
		locs   []Location
	}{
		"no levels":            {nil, nil},
		"empty level name":     {[]string{""}, nil},
		"duplicate level":      {[]string{"region", "region"}, nil},
		"empty id":             {[]string{"region"}, []Location{{Level: "region"}}},
		"duplicate id":         {[]string{"region"}, []Location{{ID: "a", Level: "region"}, {ID: "a", Level: "region"}}},
		"unknown level":        {[]string{"region"}, []Location{{ID: "a", Level: "az"}}},
		"top with parent":      {[]string{"region", "az"}, []Location{{ID: "a", Level: "region", Parent: "x"}}},
		"child without parent": {[]string{"region", "az"}, []Location{{ID: "a", Level: "az"}}},
		"unknown parent":       {[]string{"region", "az"}, []Location{{ID: "a", Level: "az", Parent: "x"}}},
		"parent at wrong level": {[]string{"region", "az", "rack"}, []Location{
			{ID: "r", Level: "region"}, {ID: "k", Level: "rack", Parent: "r"}}},
	}
	for name, tc := range tests {
		if _, err := New(tc.levels, tc.locs); err == nil {
			t.Errorf("%s: New must fail", name)
		}
	}
}
