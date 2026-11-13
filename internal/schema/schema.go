// Package schema compiles and applies the JSON Schemas that app manifests carry: action inputs
// and installation configuration (ADR-0001 D2, D4). Schemas may not load anything from the network.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// FileFormat marks an action input field that takes one of the member's files by its id (ADR-0001 D23): a string
// property, or an array of them.
const FileFormat = "idp-file"

var fileRefRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// FileFields returns the top-level properties of an object schema that take files (by name, true = an array of them).
func FileFields(raw json.RawMessage) map[string]bool {
	var s struct {
		Properties map[string]struct {
			Format string `json:"format"`
			Type   any    `json:"type"`
			Items  *struct {
				Format string `json:"format"`
			} `json:"items"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	out := map[string]bool{}
	for name, p := range s.Properties {
		switch {
		case p.Format == FileFormat:
			out[name] = false
		case p.Type == "array" && p.Items != nil && p.Items.Format == FileFormat:
			out[name] = true
		}
	}
	return out
}

// FileRefs returns the file ids an input gives in the schema's file fields, in field-name order then array order, without
// repeats.
func FileRefs(schemaRaw, input json.RawMessage) []string {
	fields := FileFields(schemaRaw)
	if len(fields) == 0 {
		return nil
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, n := range names {
		raw, ok := in[n]
		if !ok {
			continue
		}
		if fields[n] {
			var list []string
			_ = json.Unmarshal(raw, &list)
			for _, s := range list {
				add(s)
			}
		} else {
			var s string
			_ = json.Unmarshal(raw, &s)
			add(s)
		}
	}
	return out
}

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
	c.RegisterFormat(&jsonschema.Format{Name: FileFormat, Validate: func(v any) error {
		if s, ok := v.(string); ok && !fileRefRE.MatchString(s) {
			return errors.New("not a file reference (the id of one of your files)")
		}
		return nil
	}})
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
