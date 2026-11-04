// Package schema compiles and applies the JSON Schemas that app manifests carry: action inputs
// and installation configuration (ADR-0001 D2, D4). Schemas may not load anything from the network.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Compile compiles one inline schema; it must describe an object. References outside the schema
// itself are refused.
func Compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("schema is required")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schema is not JSON: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["type"] != "object" {
		return nil, errors.New(`schema must be an object schema ("type": "object")`)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	c.AssertFormat()
	if err := c.AddResource("urn:idp:workspace:inline", doc); err != nil {
		return nil, err
	}
	s, err := c.Compile("urn:idp:workspace:inline")
	if err != nil {
		return nil, fmt.Errorf("schema does not compile: %s", firstLine(err.Error()))
	}
	return s, nil
}

type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema references %s; only references inside the schema are allowed", url)
}

// Validate applies a compiled schema to a JSON document.
func Validate(s *jsonschema.Schema, doc json.RawMessage) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if err := s.Validate(v); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(describe(ve))
		}
		return err
	}
	return nil
}

var printer = message.NewPrinter(language.English)

// describe flattens a validation error to its deepest causes, one line each.
func describe(ve *jsonschema.ValidationError) string {
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			out = append(out, loc+": "+firstLine(e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) > 5 {
		out = append(out[:5], fmt.Sprintf("… and %d more", len(out)-5))
	}
	return strings.Join(out, "; ")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
