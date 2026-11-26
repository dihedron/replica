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

func (p painter) table(headers []string, rows [][]string) string {
	t := table.New().Border(lipgloss.RoundedBorder()).Headers(headers...).Rows(rows...)
	if !p.on {
		t = t.BorderStyle(lipgloss.NewStyle())
	} else {
		t = t.BorderStyle(lipgloss.NewStyle().Faint(true))
	}
	return t.StyleFunc(func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return p.style(true, "", "")
		}
		name := headers[col]
		if name == "VALUE" { // a table of fields: the row's field names the value
			name = rows[row][0]
		}
		return p.style(false, name, rows[row][col])
	}).String()
}

// list is a table of rows (JSON objects) in columns.
func (p painter) list(rows []any, columns []string) string {
	headers := make([]string, len(columns))
	for i, c := range columns {
		headers[i] = strings.ToUpper(c)
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		line := make([]string, len(columns))
		for i, c := range columns {
			line[i] = text(row[c])
		}
		cells = append(cells, line)
	}
	return p.table(headers, cells)
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

// object is a table of an object's fields; each list of objects in it is a
// table of its own, below.
func (p painter) object(fields map[string]any) string {
	var rows [][]string
	var below []string
	for _, k := range ordered(fields) {
		if list, ok := fields[k].([]any); ok && len(list) > 0 && isObjects(list) {
			cols := map[string]any{}
			for _, r := range list {
				for c := range r.(map[string]any) {
					cols[c] = nil
				}
			}
			below = append(below, strings.ToUpper(k)+"\n"+p.list(list, ordered(cols)))
			continue
		}
		rows = append(rows, []string{k, text(fields[k])})
	}
	out := p.table([]string{"FIELD", "VALUE"}, rows)
	for _, b := range below {
		out += "\n" + b
	}
	return out
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
