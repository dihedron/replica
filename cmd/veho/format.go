package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// machine is true for the formats that scripts read: they get the whole
// message, never a command's own text form.
func (o *apiOptions) machine() bool { return o.Format == "json" || o.Format == "yaml" }

// colour says whether w may get colours: a terminal, without NO_COLOR
// (.specs/veho-cli.md §3); a variable for the tests.
var colour = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// print writes msg in the chosen format: json and yaml the whole message;
// table a table of its list's columns, or of its fields when no columns
// are given.
func (o *apiOptions) print(msg proto.Message, columns ...string) error {
	b, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: o.Format == "json", Indent: "  "}.Marshal(msg)
	if err != nil {
		return err
	}
	w := o.stdout()
	if o.Format == "json" {
		_, err := fmt.Fprintln(w, string(b))
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if o.Format == "yaml" {
		y, err := yaml.Marshal(fields)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(w, "---\n"+string(y))
		return err
	}
	p := painter{on: colour(w)}
	if len(columns) > 0 {
		var rows []any
		for _, v := range fields {
			if list, ok := v.([]any); ok {
				rows = list
			}
		}
		if o.Wide {
			columns = allColumns(rows, columns)
		}
		_, err := fmt.Fprintln(w, p.list(rows, columns))
		return err
	}
	_, err = fmt.Fprintln(w, p.object(fields))
	return err
}

// painter turns JSON values into grids, coloured when on.
type painter struct{ on bool }

// The colours: bold cyan headers, green and red states, yellow the others
// of a job or transfer.
const (
	headerColour  = "\x1b[1;36m"
	pendingColour = "\x1b[33m"
)

var stateColours = map[string]string{
	"SUCCEEDED": "\x1b[32m", "active": "\x1b[32m",
	"FAILED": "\x1b[31m", "disabled": "\x1b[31m", "CANCELLED": "\x1b[31m",
}

// stateColour is the colour of a value of the field or column name.
func stateColour(name, value string) string {
	if name != "state" {
		return ""
	}
	if c, ok := stateColours[value]; ok {
		return c
	}
	if value != "" && value != "-" {
		return pendingColour
	}
	return ""
}

func header(columns []string) []gridCell {
	row := make([]gridCell, len(columns))
	for i, c := range columns {
		row[i] = gridCell{text: strings.ToUpper(c), colour: headerColour}
	}
	return row
}

// list is a table of rows (JSON objects) in columns: nested values are
// tables in their cells, as in an object's.
func (p painter) list(rows []any, columns []string) string {
	g := &grid{rows: [][]gridCell{header(columns)}}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		line := make([]gridCell, len(columns))
		for i, c := range columns {
			line[i] = p.value(c, row[c])
		}
		g.rows = append(g.rows, line)
	}
	return g.render(p.on)
}

// first are the fields shown before the others, which are sorted.
var first = map[string]int{"id": 1, "name": 2, "state": 3, "endpoint": 4, "path": 5}

func ordered(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := first[keys[i]], first[keys[j]]
		switch {
		case a != 0 && b != 0:
			return a < b
		case a != 0 || b != 0:
			return a != 0
		}
		return keys[i] < keys[j]
	})
	return keys
}

// object is a table of an object's fields under a FIELD/VALUE header.
func (p painter) object(fields map[string]any) string {
	g := p.fields(fields)
	g.rows = append([][]gridCell{header([]string{"field", "value"})}, g.rows...)
	return g.render(p.on)
}

// fields is a grid of an object's fields and values.
func (p painter) fields(fields map[string]any) *grid {
	g := &grid{}
	for _, k := range ordered(fields) {
		g.rows = append(g.rows, []gridCell{{text: k}, p.value(k, fields[k])})
	}
	return g
}

// value is the cell of a field: a nested object is a grid of its fields, a
// list of objects a grid with a row per object under its columns' header,
// and so on within them; anything else its text.
func (p painter) value(name string, v any) gridCell {
	switch x := v.(type) {
	case map[string]any:
		if len(x) > 0 {
			return gridCell{sub: p.fields(x)}
		}
	case []any:
		if len(x) > 0 && isObjects(x) {
			columns := allColumns(x, nil)
			g := &grid{rows: [][]gridCell{header(columns)}}
			for _, e := range x {
				row := make([]gridCell, len(columns))
				for i, c := range columns {
					row[i] = p.value(c, e.(map[string]any)[c])
				}
				g.rows = append(g.rows, row)
			}
			return gridCell{sub: g}
		}
	}
	t := text(v)
	return gridCell{text: t, colour: stateColour(name, t)}
}

// allColumns are every field of rows, the main columns first in their
// order, then the others as an object's fields are ordered (--wide).
func allColumns(rows []any, main []string) []string {
	seen, rest := map[string]bool{}, map[string]any{}
	for _, c := range main {
		seen[c] = true
	}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		for c := range row {
			if !seen[c] {
				rest[c] = nil
			}
		}
	}
	return append(append([]string{}, main...), ordered(rest)...)
}

func isObjects(list []any) bool {
	for _, v := range list {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// text is a cell: a nested object as its key=value pairs, a list joined.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case map[string]any:
		parts := make([]string, 0, len(x))
		for _, k := range ordered(x) {
			parts = append(parts, k+"="+text(x[k]))
		}
		return strings.Join(parts, " ")
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = text(e)
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(x)
	}
}
