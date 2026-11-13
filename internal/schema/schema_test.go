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

func TestFileFieldsTakeTheMembersFiles(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"},"files":{"type":"array","items":{"type":"string","format":"idp-file"}},"cover":{"type":"string","format":"idp-file"}},"required":["message"]}`)
	s, err := Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	a, b := "6f1c2a9e-8d4b-4c3a-9e21-0b7d5f3a8c41", "7a2d3b0f-9e5c-4d4b-8f32-1c8e6a4b9d52"
	ok := json.RawMessage(`{"message":"x","files":["` + a + `","` + b + `","` + a + `"],"cover":"` + b + `"}`)
	if err := Validate(s, ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"message":"x","files":["../etc/passwd"]}`, `{"message":"x","cover":"attachment:1"}`} {
		if Validate(s, json.RawMessage(bad)) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if f := FileFields(raw); len(f) != 2 || !f["files"] || f["cover"] {
		t.Fatalf("%v", f)
	}
	// field-name order (cover before files), array order, no repeats
	if got := FileRefs(raw, ok); strings.Join(got, ",") != b+","+a {
		t.Fatalf("%v", got)
	}
	if FileRefs(json.RawMessage(`{"type":"object"}`), ok) != nil {
		t.Fatal("refs from a schema without file fields")
	}
}
