package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"idp-workspace/internal/apptoken"
	"idp-workspace/internal/files"
	"idp-workspace/internal/workspace"
)

func ticketer(t *testing.T) *files.Ticketer {
	t.Helper()
	s, err := apptoken.Generate("https://workspace.example")
	if err != nil {
		t.Fatal(err)
	}
	return &files.Ticketer{Signer: s, TTL: time.Hour}
}

func withFiles(r Request) Request {
	r.Input = json.RawMessage(`{"message":"把报价改成第二版","files":["6f1c2a9e-8d4b-4c3a-9e21-0b7d5f3a8c41"]}`)
	r.FileFields = map[string]bool{"files": true}
	r.Files = []workspace.MemberFile{{ID: "6f1c2a9e-8d4b-4c3a-9e21-0b7d5f3a8c41", Name: "报价单.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Size: 48213, SHA256: strings.Repeat("3b", 32)}}
	return r
}

// D23: a host.run with files gives idp-host each file with a read ticket and a write ticket for deliveries, as RunOptions
// of the pinned idp.telos.host 1.8.0.
func TestHostRunGivesTheFilesWithTickets(t *testing.T) {
	var got seen
	srv := fake(t, 202, `{"runId":"ws_11111111222243338444555555555555","threadId":"thr_1","status":"QUEUED"}`, &got)
	tk := ticketer(t)
	d := &Dispatcher{Host: &Downstream{Endpoint: srv.URL, Auth: AuthForward}, Tickets: tk, DeliverFiles: 3, DeliverBytes: 1 << 20}
	r := withFiles(req(workspace.Target{Kind: workspace.TargetHostRun, Agent: "office-assistant"}, ""))
	r.Principal.TokenSubject = "alice-sub" // the token's sub, when the workspace names members by another claim
	if _, f := d.Dispatch(context.Background(), r); f != nil {
		t.Fatal(f)
	}
	if m := got.body["messages"].([]any)[0].(map[string]any)["content"]; m != "把报价改成第二版" {
		t.Fatalf("the message is %v", m)
	}
	props := got.body["forwardedProps"].(map[string]any)
	atts := props["idp.host/attachments"].([]any)
	a := atts[0].(map[string]any)
	if len(atts) != 1 || a["ref"] != r.Files[0].ID || a["name"] != "报价单.docx" || a["size"] != 48213.0 {
		t.Fatalf("%v", atts)
	}
	read, err := tk.Open(a["ticket"].(string))
	if err != nil || read.Kind != files.KindRead || read.File != r.Files[0].ID || read.Owner != "alice" || read.Caller != "alice-sub" || read.Tenant != "t1" || read.Run != "ws_11111111222243338444555555555555" {
		t.Fatalf("%+v %v", read, err)
	}
	write, err := tk.Open(props["idp.host/delivery"].(map[string]any)["ticket"].(string))
	if err != nil || write.Kind != files.KindWrite || write.Workspace != "w1" || write.MaxFiles != 3 || write.MaxBytes != 1<<20 || write.Run != read.Run {
		t.Fatalf("%+v %v", write, err)
	}
	// what the workspace sends is RunOptions of the contract it pins
	raw, err := os.ReadFile("../../tests/contracts/idp.telos.host/1.8.0/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	c := jsonschema.NewCompiler()
	if err := c.AddResource("urn:idp:telos:host:1.8.0", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("urn:idp:telos:host:1.8.0#/$defs/RunOptions")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(props)
	v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err := s.Validate(v); err != nil {
		t.Fatalf("the workspace's run options are not idp.telos.host 1.8.0 RunOptions: %v", err)
	}
}

func TestHostRunWithoutTicketsTakesNoFiles(t *testing.T) {
	var got seen
	srv := fake(t, 202, `{"runId":"r","threadId":"t","status":"QUEUED"}`, &got)
	d := &Dispatcher{Host: &Downstream{Endpoint: srv.URL, Auth: AuthForward}}
	if _, f := d.Dispatch(context.Background(), withFiles(req(workspace.Target{Kind: workspace.TargetHostRun, Agent: "a"}, ""))); f == nil || f.Code != "TARGET_UNSUPPORTED" {
		t.Fatalf("files went to a run without tickets: %v", f)
	}
	if _, f := d.Dispatch(context.Background(), req(workspace.Target{Kind: workspace.TargetHostRun, Agent: "a"}, `{"message":"hi"}`)); f != nil {
		t.Fatal(f)
	}
	if _, ok := got.body["forwardedProps"]; ok {
		t.Fatalf("file options without a ticketer: %v", got.body)
	}
	// with a ticketer and no file, the run may still deliver
	d.Tickets = ticketer(t)
	if _, f := d.Dispatch(context.Background(), req(workspace.Target{Kind: workspace.TargetHostRun, Agent: "a"}, `{"message":"hi"}`)); f != nil {
		t.Fatal(f)
	}
	props := got.body["forwardedProps"].(map[string]any)
	if _, ok := props["idp.host/attachments"]; ok || props["idp.host/delivery"] == nil {
		t.Fatalf("%v", props)
	}
}
