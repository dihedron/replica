package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
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

// painter draws the tables, in colour when on.
type painter struct{ on bool }

var (
	headerColour = lipgloss.Color("6") // cyan
	stateColours = map[string]string{
		"SUCCEEDED": "2", "active": "2", "true": "2",
		"FAILED": "1", "disabled": "1", "CANCELLED": "1",
	}
	pending = lipgloss.Color("3") // the other states of a job or transfer
)

// cell styles a cell of the column col: the header, and the states.
func (p painter) style(header bool, col, value string) lipgloss.Style {
	s := lipgloss.NewStyle().Padding(0, 1)
	if !p.on {
		return s
	}
	if header {
		return s.Bold(true).Foreground(headerColour)
	}
	if col == "state" || col == "STATE" {
		if c, ok := stateColours[value]; ok {
			return s.Foreground(lipgloss.Color(c))
		}
		if value != "" && value != "-" {
			return s.Foreground(pending)
		}
	}
	return s
}

// table draws rows under headers (none when nil); in a table of fields
// (kv), a value is coloured as its row's field names it.
func (p painter) table(headers []string, rows [][]string, kv bool) string {
	t := table.New().Border(lipgloss.RoundedBorder()).Rows(rows...)
	if headers != nil {
		t = t.Headers(headers...)
	}
	if !p.on {
		t = t.BorderStyle(lipgloss.NewStyle())
	} else {
		t = t.BorderStyle(lipgloss.NewStyle().Faint(true))
	}
	return t.StyleFunc(func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return p.style(true, "", "")
		}
		name := ""
		switch {
		case kv && col == 1:
			name = rows[row][0]
		case !kv:
			name = headers[col]
		}
		return p.style(false, name, rows[row][col])
	}).String()
}

// list is a table of rows (JSON objects) in columns, one line each: nested
// values are their text.
func (p painter) list(rows []any, columns []string) string { return p.rows(rows, columns, text) }

// rows is list with each cell drawn by cell: embedded in a table of fields,
// p.value, so that nested values are tables in their turn.
func (p painter) rows(rows []any, columns []string, cell func(any) string) string {
	headers := make([]string, len(columns))
	for i, c := range columns {
		headers[i] = strings.ToUpper(c)
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		line := make([]string, len(columns))
		for i, c := range columns {
			line[i] = cell(row[c])
		}
		cells = append(cells, line)
	}
	return p.table(headers, cells, false)
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

// object is a table of an object's fields.
func (p painter) object(fields map[string]any) string { return p.fields(fields, true) }

// fields is a table of fields and values, with its FIELD/VALUE header or,
// embedded in another table's cell, without.
func (p painter) fields(fields map[string]any, header bool) string {
	rows := make([][]string, 0, len(fields))
	for _, k := range ordered(fields) {
		rows = append(rows, []string{k, p.value(fields[k])})
	}
	var headers []string
	if header {
		headers = []string{"FIELD", "VALUE"}
	}
	return p.table(headers, rows, true)
}

// value is a cell of a table of fields: a nested object, or a list of
// objects, is a table embedded in it; anything else its text.
func (p painter) value(v any) string {
	switch x := v.(type) {
	case map[string]any:
		if len(x) > 0 {
			return p.fields(x, false)
		}
	case []any:
		if len(x) > 0 && isObjects(x) {
			return p.rows(x, allColumns(x, nil), p.value)
		}
	}
	return text(v)
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
