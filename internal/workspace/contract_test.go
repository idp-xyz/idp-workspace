package workspace_test

// The published contract idp.telos.workspace (idp-contracts/idp.telos.workspace/<version>) is
// produced here. api/contracts/schema.json is written by hand; these tests prove it describes exactly
// the objects this package defines and the API serves. api/contracts/examples.json is generated from
// Go values (go test ./internal/workspace -run TestContract -update) and checked on every run.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"idp-workspace/internal/workspace"
)

var update = flag.Bool("update", false, "regenerate api/contracts/examples.json from the Go values")

const (
	contractDir = "../../api/contracts"
	contractID  = "urn:idp:telos:workspace:1.2.0"
)

type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external reference %q is not loaded", url)
}

type contract struct {
	t    *testing.T
	c    *jsonschema.Compiler
	defs map[string]*jsonschema.Schema
	raw  map[string]any
}

func loadContract(t *testing.T) *contract {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	c.AssertFormat()
	if err := c.AddResource(contractID, doc); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	return &contract{t: t, c: c, defs: map[string]*jsonschema.Schema{}, raw: raw}
}

func (k *contract) schema(def string) *jsonschema.Schema {
	if s := k.defs[def]; s != nil {
		return s
	}
	loc := contractID
	if def != "" {
		loc += "#/$defs/" + def
	}
	s, err := k.c.Compile(loc)
	if err != nil {
		k.t.Fatalf("compile %s: %v", loc, err)
	}
	k.defs[def] = s
	return s
}

var roots = map[string]bool{"Workspace": true, "AppManifest": true, "AppRelease": true, "Installation": true, "InboxItem": true, "WorkContext": true, "Invocation": true, "BridgeMessage": true, "Problem": true, "MemberFile": true}

func (k *contract) validate(def string, raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := k.schema(def).Validate(inst); err != nil {
		return fmt.Errorf("%s: %w", def, err)
	}
	if roots[def] {
		if err := k.schema("").Validate(inst); err != nil {
			return fmt.Errorf("root oneOf rejects a valid %s: %w", def, err)
		}
	}
	return nil
}

var at = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

const (
	wsID   = "0a1b2c3d-0000-4000-8000-000000000001"
	itemID = "1b2c3d4e-0000-4000-8000-000000000002"
	instID = "2c3d4e5f-0000-4000-8000-000000000003"
	invID  = "3d4e5f60-0000-4000-8000-000000000004"
	inv2ID = "4e5f6071-0000-4000-8000-000000000005"
)

func exampleManifest() workspace.Manifest {
	return workspace.Manifest{SchemaVersion: workspace.ManifestSchemaVersion, Name: "orders", Version: "1.2.0", Title: "Orders", Description: "Order desk: holds, reviews and approvals.",
		Publisher: "IDP Parcel", Entry: &workspace.Entry{URL: "https://orders.apps.example/workspace"},
		Capabilities: []string{workspace.CapContextRead, workspace.CapContextWrite, workspace.CapActionsInvoke, workspace.CapInboxRead, workspace.CapNotify, workspace.CapIdentityToken},
		Actions: []workspace.ActionContract{
			{Name: "order.review", Title: "Ask the order assistant", Input: json.RawMessage(`{"type":"object","required":["message"],"properties":{"message":{"type":"string","minLength":1,"maxLength":4000}},"additionalProperties":false}`),
				Target: workspace.Target{Kind: workspace.TargetHostRun, Agent: "order-assistant"}},
			{Name: "approval.approve", Title: "Approve", Input: json.RawMessage(`{"type":"object","properties":{"executionId":{"type":"string"}},"additionalProperties":false}`),
				Target: workspace.Target{Kind: workspace.TargetEERDecision, Decision: "approve"}, Roles: []string{"approver"}, Confirm: true},
			{Name: "approval.reject", Title: "Reject", Input: json.RawMessage(`{"type":"object","properties":{"executionId":{"type":"string"}},"additionalProperties":false}`),
				Target: workspace.Target{Kind: workspace.TargetEERDecision, Decision: "reject"}, Roles: []string{"approver"}, Confirm: true},
			{Name: "order.hold", Title: "Put an order on hold", Input: json.RawMessage(`{"type":"object","required":["orderId","reason"],"properties":{"orderId":{"type":"string","pattern":"^[0-9]+$"},"reason":{"type":"string","maxLength":500}},"additionalProperties":false}`),
				Target: workspace.Target{Kind: workspace.TargetWebhook, URL: "https://orders.apps.example/hooks/hold"}}},
		Config: &workspace.ConfigSpec{Schema: json.RawMessage(`{"type":"object","required":["region"],"properties":{"region":{"enum":["us","eu"]}},"additionalProperties":false}`)}}
}

func exampleItem() workspace.InboxItem {
	due := at.Add(4 * time.Hour)
	return workspace.InboxItem{ID: itemID, WorkspaceID: wsID, Producer: "svc-eer", ExternalRef: "execution:7f3a/approval", Kind: workspace.ItemApproval,
		Title: "Approve the refund for order 1001", Summary: "Refund of 420.00 USD exceeds the agent's limit.",
		Ref:       workspace.Ref{System: "eer", Kind: "execution", ID: "execution:7f3a", URL: "https://eer.example/executions/execution:7f3a"},
		Assignees: workspace.Assignees{Subjects: []string{}, Roles: []string{"approver"}}, Priority: "high", DueAt: &due,
		Actions:  []workspace.ItemAction{{App: "orders", Action: "approval.approve", Label: "Approve"}, {App: "orders", Action: "approval.reject", Label: "Reject"}},
		Entities: []workspace.Entity{{Type: "order", ID: "1001"}}, Status: workspace.ItemOpen, Version: 1, CreatedAt: at, UpdatedAt: at}
}

type example struct {
	Definition string          `json:"definition"`
	Value      json.RawMessage `json:"value"`
}

type examplesFile struct {
	Description string    `json:"description"`
	GeneratedBy string    `json:"generatedBy"`
	Examples    []example `json:"examples"`
}

func buildExamples(t *testing.T) []byte {
	t.Helper()
	var xs []example
	add := func(def string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		xs = append(xs, example{def, raw})
	}
	add("Workspace", workspace.Workspace{ID: wsID, Name: "us-ops", DisplayName: "US operations", CreatedBy: "alice", CreatedAt: at})
	m := exampleManifest()
	add("AppManifest", m)
	add("AppManifest", workspace.Manifest{SchemaVersion: workspace.ManifestSchemaVersion, Name: "notifier", Version: "0.1.0", Title: "Notifier", Publisher: "IDP",
		Capabilities: []string{}, Actions: []workspace.ActionContract{{Name: "notify", Title: "Notify the on-call", Input: json.RawMessage(`{"type":"object"}`),
			Target: workspace.Target{Kind: workspace.TargetWebhook, URL: "http://127.0.0.1:9000/notify"}}}})
	add("AppRelease", workspace.Release{Name: m.Name, Version: m.Version, Digest: "sha256:" + strings.Repeat("ab", 32), Status: "published", Manifest: m, PublishedBy: "alice", PublishedAt: at})
	add("AppRelease", workspace.Release{Name: m.Name, Version: "1.1.0", Digest: "sha256:" + strings.Repeat("cd", 32), Status: "revoked", Manifest: func() workspace.Manifest { x := exampleManifest(); x.Version = "1.1.0"; return x }(),
		PublishedBy: "alice", PublishedAt: at.Add(-72 * time.Hour), Revocation: &workspace.Revocation{By: "alice", At: at.Add(-time.Hour), Reason: "approves without the amount check"}})
	add("Installation", workspace.Installation{ID: instID, WorkspaceID: wsID, App: "orders", Version: "1.2.0", Config: json.RawMessage(`{"region":"us"}`), Revision: 2,
		InstalledBy: "alice", InstalledAt: at.Add(-24 * time.Hour), UpdatedAt: at, ReleaseStatus: "published"})
	add("InboxItem", exampleItem())
	read := at.Add(time.Hour)
	done := exampleItem()
	done.Status, done.Version, done.UpdatedAt, done.ReadAt = workspace.ItemResolved, 2, at.Add(2*time.Hour), &read
	done.Resolution = &workspace.Resolution{Outcome: "approved", By: "svc-eer", At: at.Add(2 * time.Hour), Note: "approved by bob in the workspace"}
	add("InboxItem", done)
	add("WorkContext", workspace.WorkContext{WorkspaceID: wsID, Subject: "bob", Entities: []workspace.Entity{{Type: "order", ID: "1001"}, {Type: "customer", ID: "carol"}},
		App: "orders", Thread: "thr_ops_42", ItemID: itemID, Version: 3, UpdatedAt: at})
	add("Invocation", workspace.Invocation{ID: invID, WorkspaceID: wsID, Subject: "bob", App: "orders", AppVersion: "1.2.0", Action: "approval.approve", ItemID: itemID,
		InputDigest: "sha256:" + strings.Repeat("0f", 32), Target: workspace.Target{Kind: workspace.TargetEERDecision, Decision: "approve"}, Status: workspace.InvocationSucceeded,
		Result: json.RawMessage(`{"accepted":true,"decision":"approved","httpStatus":202,"id":"execution:7f3a","kind":"execution","system":"eer"}`), CreatedAt: at.Add(time.Hour)})
	add("Invocation", workspace.Invocation{ID: inv2ID, WorkspaceID: wsID, Subject: "bob", App: "orders", AppVersion: "1.2.0", Action: "order.review",
		InputDigest: "sha256:" + strings.Repeat("1e", 32), Target: workspace.Target{Kind: workspace.TargetHostRun, Agent: "order-assistant"}, Status: workspace.InvocationFailed,
		Error: &workspace.Problem{Code: "TARGET_UNAVAILABLE", Message: "https://host.example unreachable"}, CreatedAt: at.Add(time.Hour)})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, ID: "1", Type: workspace.BridgeRequest, Method: "bridge.hello", Params: json.RawMessage(`{"app":"orders","version":"1.2.0"}`)})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, ID: "1", Type: workspace.BridgeResponse,
		Result: json.RawMessage(`{"capabilities":["context.read","actions.invoke"],"locale":"zh-CN","protocol":"idp.bridge/v1","theme":"light","workspace":{"name":"us-ops","workspaceId":"` + wsID + `"}}`)})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, ID: "2", Type: workspace.BridgeRequest, Method: "actions.invoke", Params: json.RawMessage(`{"action":"order.hold","input":{"orderId":"1001","reason":"address check"}}`)})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, ID: "3", Type: workspace.BridgeResponse, Error: &workspace.Problem{Code: "CAPABILITY_DENIED", Message: "the app did not request inbox.read"}})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, ID: "4", Type: workspace.BridgeRequest, Method: "identity.token"})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, ID: "4", Type: workspace.BridgeResponse, Result: json.RawMessage(`{"expiresAt":"2026-11-04T09:05:00Z","token":"eyJhbGciOiJFUzI1NiJ9.e30.c2ln"}`)})
	add("BridgeMessage", workspace.BridgeMessage{Protocol: workspace.BridgeProtocol, Type: workspace.BridgeEvent, Event: "context.changed", Data: json.RawMessage(`{"entities":[{"id":"1002","type":"order"}],"version":4}`)})
	add("Problem", workspace.Problem{Code: "ACTION_NOT_OFFERED", Message: "the item does not offer orders/order.hold"})
	// D23: a file bob uploaded, and the file a run delivered to him
	add("MemberFile", workspace.MemberFile{ID: "6f1c2a9e-8d4b-4c3a-9e21-0b7d5f3a8c41", Workspace: "us-ops", Name: "报价单.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Size: 48213, SHA256: strings.Repeat("3b", 32), Source: workspace.FileSourceUpload, Sensitivity: "internal", CreatedAt: at, ExpiresAt: at.Add(30 * 24 * time.Hour)})
	add("MemberFile", workspace.MemberFile{ID: "7a2d3b0f-9e5c-4d4b-8f32-1c8e6a4b9d52", Workspace: "us-ops", Name: "报价单-修订.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Size: 49102, SHA256: strings.Repeat("4c", 32), Source: "run:ws_7f3a1c2e5b6d4c8e9f0123456789abcd", Sensitivity: "internal", CreatedAt: at.Add(time.Hour), ExpiresAt: at.Add(30*24*time.Hour + time.Hour)})
	f := examplesFile{Description: "idp.telos.workspace 1.2.0 的示例：每一个都是 idp-workspace 的 Go 值序列化的结果（API 返回或应用桥发送的形状）。", GeneratedBy: "go test ./internal/workspace -run TestContract -update", Examples: xs}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestContractExamplesAreGeneratedAndValid(t *testing.T) {
	want := buildExamples(t)
	path := filepath.Join(contractDir, "examples.json")
	if *update {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, want) {
		t.Fatalf("%s is out of date; run: go test ./internal/workspace -run TestContract -update", path)
	}
	var f examplesFile
	if err := json.Unmarshal(have, &f); err != nil {
		t.Fatal(err)
	}
	k := loadContract(t)
	seen := map[string]int{}
	for i, x := range f.Examples {
		seen[x.Definition]++
		if err := k.validate(x.Definition, x.Value); err != nil {
			t.Errorf("example %d (%s): %v", i, x.Definition, err)
		}
		if x.Definition == "AppManifest" {
			var m workspace.Manifest
			dec := json.NewDecoder(bytes.NewReader(x.Value))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&m); err != nil {
				t.Errorf("example %d: %v", i, err)
			} else if err := m.Validate(); err != nil {
				t.Errorf("example %d: the implementation rejects the published manifest: %v", i, err)
			}
		}
	}
	for def := range roots {
		if seen[def] == 0 {
			t.Errorf("no example of %s", def)
		}
	}
}

// TestContractFieldsMatchGo: every definition is closed, its properties are exactly the struct's
// JSON fields, and its required list is exactly the fields serialized without omitempty.
func TestContractFieldsMatchGo(t *testing.T) {
	k := loadContract(t)
	defs := k.raw["$defs"].(map[string]any)
	closed := map[string]any{"Workspace": workspace.Workspace{}, "AppManifest": workspace.Manifest{}, "AppEntry": workspace.Entry{}, "ConfigSpec": workspace.ConfigSpec{},
		"ActionContract": workspace.ActionContract{}, "Target": workspace.Target{}, "Revocation": workspace.Revocation{}, "AppRelease": workspace.Release{},
		"Installation": workspace.Installation{}, "Ref": workspace.Ref{}, "Assignees": workspace.Assignees{}, "ItemAction": workspace.ItemAction{},
		"Resolution": workspace.Resolution{}, "Entity": workspace.Entity{}, "InboxItem": workspace.InboxItem{}, "WorkContext": workspace.WorkContext{},
		"Problem": workspace.Problem{}, "Invocation": workspace.Invocation{}, "BridgeMessage": workspace.BridgeMessage{}, "MemberFile": workspace.MemberFile{}}
	for def, zero := range closed {
		d, ok := defs[def].(map[string]any)
		if !ok {
			t.Errorf("definition %s missing", def)
			continue
		}
		if d["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties must be false", def)
		}
		var fields, required []string
		rt := reflect.TypeOf(zero)
		for i := 0; i < rt.NumField(); i++ {
			name, opts, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name == "-" || name == "" {
				continue
			}
			fields = append(fields, name)
			if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
				required = append(required, name)
			}
		}
		sort.Strings(fields)
		sort.Strings(required)
		var props, req []string
		for name := range d["properties"].(map[string]any) {
			props = append(props, name)
		}
		for _, r := range d["required"].([]any) {
			req = append(req, r.(string))
		}
		sort.Strings(props)
		sort.Strings(req)
		if !reflect.DeepEqual(props, fields) {
			t.Errorf("%s: schema properties %v, Go fields %v", def, props, fields)
		}
		if !reflect.DeepEqual(req, required) {
			t.Errorf("%s: schema required %v, Go always-serialized %v", def, req, required)
		}
	}
	if len(defs) != len(closed) {
		t.Errorf("schema has %d definitions, the test lists %d", len(defs), len(closed))
	}
}

func set(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	var cur any = m
	for i, p := range parts {
		last := i == len(parts)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				if v == nil {
					delete(c, p)
				} else {
					c[p] = v
				}
				return
			}
			cur = c[p]
		case []any:
			n, _ := strconv.Atoi(p)
			if last {
				c[n] = v
				return
			}
			cur = c[n]
		}
	}
}

func mutate(t *testing.T, v any, path string, value any) []byte {
	t.Helper()
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	set(m, path, value)
	out, _ := json.Marshal(m)
	return out
}

// TestContractRejectsWhatTheWorkspaceRejects: documents the implementation refuses, the schema
// refuses too (manifests are checked against Manifest.Validate as well).
func TestContractRejectsWhatTheWorkspaceRejects(t *testing.T) {
	k := loadContract(t)
	m := exampleManifest()
	manifestCases := map[string]struct {
		path  string
		value any
	}{
		"wrong schemaVersion":    {"schemaVersion", "idp.workspace.app/v2"},
		"uppercase name":         {"name", "Orders"},
		"not semver":             {"version", "1.2"},
		"empty title":            {"title", ""},
		"plain http entry":       {"entry.url", "http://orders.example/"},
		"entry with fragment":    {"entry.url", "https://orders.example/#x"},
		"unknown capability":     {"capabilities.0", "storage.write"},
		"bad action name":        {"actions.0.name", "Order Review"},
		"input not object":       {"actions.0.input", map[string]any{"type": "string"}},
		"unknown target":         {"actions.0.target.kind", "email"},
		"host.run without agent": {"actions.0.target", map[string]any{"kind": "host.run"}},
		"decision maybe":         {"actions.1.target.decision", "maybe"},
		"webhook without url":    {"actions.3.target", map[string]any{"kind": "webhook"}},
		"mixed target":           {"actions.3.target", map[string]any{"kind": "webhook", "url": "https://x.example/h", "agent": "a"}},
		"bad role":               {"actions.1.roles.0", "Approver"},
		"config not object":      {"config.schema", map[string]any{"type": "array"}},
		"missing publisher":      {"publisher", nil},
		"unknown field":          {"extra", true},
	}
	for name, c := range manifestCases {
		raw := mutate(t, m, c.path, c.value)
		if err := k.validate("AppManifest", raw); err == nil {
			t.Errorf("manifest %s: schema accepts it", name)
		}
		var back workspace.Manifest
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&back); err == nil {
			if err := back.Validate(); err == nil && name != "input not object" && name != "config not object" {
				t.Errorf("manifest %s: Manifest.Validate accepts it", name)
			}
		}
	}
	item := exampleItem()
	for name, c := range map[string]struct {
		path  string
		value any
	}{
		"unknown kind":     {"kind", "chat"},
		"bad priority":     {"priority", "critical"},
		"bad ref system":   {"ref.system", "erp"},
		"empty title":      {"title", ""},
		"unknown status":   {"status", "decided"},
		"version zero":     {"version", 0},
		"bad entity type":  {"entities.0.type", "Order"},
		"action bad app":   {"actions.0.app", "Orders"},
		"missing assignee": {"assignees", nil},
	} {
		if err := k.validate("InboxItem", mutate(t, item, c.path, c.value)); err == nil {
			t.Errorf("item %s: schema accepts it", name)
		}
	}
	for name, raw := range map[string]string{
		"request without method":   `{"protocol":"idp.bridge/v1","type":"request","id":"1"}`,
		"request with result":      `{"protocol":"idp.bridge/v1","type":"request","id":"1","method":"context.get","result":{}}`,
		"unknown method":           `{"protocol":"idp.bridge/v1","type":"request","id":"1","method":"token.get"}`,
		"response without outcome": `{"protocol":"idp.bridge/v1","type":"response","id":"1"}`,
		"response with both":       `{"protocol":"idp.bridge/v1","type":"response","id":"1","result":{},"error":{"code":"X","message":"x"}}`,
		"event with id":            `{"protocol":"idp.bridge/v1","type":"event","id":"1","event":"context.changed"}`,
		"unknown event":            `{"protocol":"idp.bridge/v1","type":"event","event":"token.changed"}`,
		"other protocol":           `{"protocol":"idp.bridge/v2","type":"event","event":"context.changed"}`,
	} {
		if err := k.validate("BridgeMessage", []byte(raw)); err == nil {
			t.Errorf("bridge %s: schema accepts it", name)
		}
	}
	for name, raw := range map[string]string{
		"failed without error":  `{"invocationId":"` + invID + `","workspaceId":"` + wsID + `","subject":"bob","app":"orders","appVersion":"1.2.0","action":"order.review","inputDigest":"sha256:` + strings.Repeat("0f", 32) + `","target":{"kind":"host.run","agent":"a"},"status":"failed","createdAt":"2026-11-04T09:00:00Z"}`,
		"succeeded with error":  `{"invocationId":"` + invID + `","workspaceId":"` + wsID + `","subject":"bob","app":"orders","appVersion":"1.2.0","action":"order.review","inputDigest":"sha256:` + strings.Repeat("0f", 32) + `","target":{"kind":"host.run","agent":"a"},"status":"succeeded","error":{"code":"X","message":"x"},"createdAt":"2026-11-04T09:00:00Z"}`,
		"digest without sha256": `{"invocationId":"` + invID + `","workspaceId":"` + wsID + `","subject":"bob","app":"orders","appVersion":"1.2.0","action":"order.review","inputDigest":"md5:00","target":{"kind":"host.run","agent":"a"},"status":"succeeded","createdAt":"2026-11-04T09:00:00Z"}`,
	} {
		if err := k.validate("Invocation", []byte(raw)); err == nil {
			t.Errorf("invocation %s: schema accepts it", name)
		}
	}
	// every bridge method and event the Go side knows is in the schema, and nothing else
	bm := k.raw["$defs"].(map[string]any)["BridgeMessage"].(map[string]any)["properties"].(map[string]any)
	enum := func(field string) []string {
		var out []string
		for _, x := range bm[field].(map[string]any)["enum"].([]any) {
			out = append(out, x.(string))
		}
		sort.Strings(out)
		return out
	}
	var methods, events []string
	for m := range workspace.BridgeMethods {
		methods = append(methods, m)
	}
	for e := range workspace.BridgeEvents {
		events = append(events, e)
	}
	sort.Strings(methods)
	sort.Strings(events)
	if !reflect.DeepEqual(enum("method"), methods) || !reflect.DeepEqual(enum("event"), events) {
		t.Errorf("bridge methods/events differ: schema %v %v, Go %v %v", enum("method"), enum("event"), methods, events)
	}
}
