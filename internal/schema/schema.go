// Package schema validates an endpoint's configuration against its
// driver's JSON Schema (.specs/veho-config-validation.md, PLG-10).
package schema

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Schema is a driver's compiled schema; nil validates everything (a
// driver that ships none).
type Schema struct {
	s *jsonschema.Schema
}

// Compile compiles a driver's schema. No reference outside it is resolved,
// whatever its scheme: a schema must not make the node fetch (CV-1).
func Compile(driver string, raw []byte) (*Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil //nolint:nilnil // no schema, nothing to check
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("driver %s: its configuration schema: %w", driver, err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(jsonschema.SchemeURLLoader{}) // no scheme at all: no file, no http
	loc := "veho:///drivers/" + driver + ".json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("driver %s: its configuration schema: %w", driver, err)
	}
	s, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("driver %s: its configuration schema: %w", driver, err)
	}
	return &Schema{s: s}, nil
}

var english = message.NewPrinter(language.English)

// Validate checks a configuration; the error names each value refused by
// its JSON path.
func (s *Schema) Validate(cfg []byte) error {
	if s == nil {
		return nil
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(cfg))
	if err != nil {
		return fmt.Errorf("the configuration is not JSON: %w", err)
	}
	err = s.s.Validate(v)
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	var out []string
	var leaves func(e *jsonschema.ValidationError)
	leaves = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, fmt.Sprintf("/%s: %s", strings.Join(e.InstanceLocation, "/"), e.ErrorKind.LocalizedString(english)))
		}
		for _, c := range e.Causes {
			leaves(c)
		}
	}
	leaves(ve)
	return errors.New(strings.Join(out, "; "))
}
