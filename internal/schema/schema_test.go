package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileAndValidate(t *testing.T) {
	s, err := Compile(json.RawMessage(`{"type":"object","required":["orderId"],"properties":{"orderId":{"type":"string","pattern":"^[0-9]+$"},"note":{"type":"string","maxLength":5}},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(s, json.RawMessage(`{"orderId":"1001"}`)); err != nil {
		t.Fatal(err)
	}
	for doc, want := range map[string]string{
		`{}`:                               "orderId",
		`{"orderId":"x"}`:                  "/orderId",
		`{"orderId":"1","extra":true}`:     "extra",
		`{"orderId":"1","note":"toolong"}`: "/note",
	} {
		err := Validate(s, json.RawMessage(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v (want mention of %s)", doc, err, want)
		}
	}
	for _, bad := range []string{``, `[]`, `{"type":"string"}`, `{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`, `{"type":"object","properties":{"a":{"type":"nope"}}}`} {
		if _, err := Compile(json.RawMessage(bad)); err == nil {
			t.Fatalf("compiled %q", bad)
		}
	}
}
