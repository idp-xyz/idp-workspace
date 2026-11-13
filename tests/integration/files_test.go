package integration

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const officeManifest = `{
  "schemaVersion": "idp.workspace.app/v1", "name": "office", "version": "1.0.0", "title": "Office", "publisher": "IDP",
  "capabilities": [],
  "actions": [{"name": "office.run", "title": "Office assistant",
    "input": {"type": "object", "properties": {"message": {"type": "string", "minLength": 1}, "files": {"type": "array", "maxItems": 10, "items": {"type": "string", "format": "idp-file"}}}, "required": ["message"], "additionalProperties": false},
    "target": {"kind": "host.run", "agent": "office-assistant"}}]
}`

func docxOf(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("word/document.xml")
	_, _ = w.Write([]byte("<w:document><w:body><w:p><w:r><w:t>" + text + "</w:t></w:r></w:p></w:body></w:document>"))
	_ = z.Close()
	return b.Bytes()
}

// raw sends bytes and returns bytes (file content, the internal endpoint); bearer is the token itself.
func raw(t *testing.T, method, u, bearer string, body []byte, headers ...string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

func memberFiles(t *testing.T, e *env) {
	upload := func(who, name string, data []byte) (int, map[string]any) {
		st, out, _ := raw(t, "POST", e.api.URL+"/v1/workspaces/us-ops/files?name="+url.QueryEscape(name), e.tokens[who], data)
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return st, m
	}
	quote := docxOf(t, "报价：第一版")
	st, f := upload("bob", "../报价单.docx", quote)
	sum := sha256.Sum256(quote)
	if st != 201 || f["name"] != "报价单.docx" || f["source"] != "upload" || f["sha256"] != hex.EncodeToString(sum[:]) || f["workspace"] != "us-ops" || f["sensitivity"] != "internal" {
		t.Fatalf("%d %v", st, f)
	}
	id := f["id"].(string)
	// what is not accepted
	for name, data := range map[string][]byte{"a.docm": quote, "a.exe": []byte("MZ"), "fake.docx": []byte("plain"), "big.txt": bytes.Repeat([]byte("a"), 1<<20+1), "": quote, "empty.txt": nil} {
		if st, out := upload("bob", name, data); st < 400 || st >= 500 {
			t.Errorf("%q accepted: %d %v", name, st, out)
		}
	}
	if st, _ := upload("dan", "x.txt", []byte("x")); st != 404 { // not a member of us-ops
		t.Fatalf("a non-member uploaded: %d", st)
	}
	// the owner lists, reads and downloads it; nobody else does
	if l := e.must(200, "GET", "/v1/workspaces/us-ops/files", "bob", nil)["files"].([]any); len(l) != 1 {
		t.Fatalf("%v", l)
	}
	if l := e.must(200, "GET", "/v1/workspaces/us-ops/files", "carol", nil)["files"].([]any); len(l) != 0 {
		t.Fatalf("carol sees bob's files: %v", l)
	}
	e.must(404, "GET", "/v1/workspaces/us-ops/files/"+id, "carol", nil)
	e.must(404, "GET", "/v1/workspaces/us-ops/files/"+id, "alice", nil) // an administrator is not the owner
	st, got, h := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+id+"/content", e.tokens["bob"], nil)
	if _, params, _ := mime.ParseMediaType(h.Get("Content-Disposition")); st != 200 || !bytes.Equal(got, quote) || params["filename"] != "报价单.docx" ||
		h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("%d %v", st, h)
	}
	if st, _, _ := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+id+"/content", e.tokens["carol"], nil); st != 404 {
		t.Fatalf("carol downloaded bob's file: %d", st)
	}
	if st, _, _ := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+id+"/content", e.tokens["erin"], nil); st != 404 {
		t.Fatalf("another tenant downloaded: %d", st)
	}

	// an action that takes files: bob's file goes to the run with a ticket, carol cannot attach it
	e.must(201, "POST", "/api/v1/apps", "alice", json.RawMessage(officeManifest))
	e.must(201, "PUT", "/api/v1/workspaces/us-ops/installations/office", "alice", map[string]any{"version": "1.0.0", "config": map[string]any{}})
	if st, out := e.call("POST", "/v1/workspaces/us-ops/actions/invoke", "carol", map[string]any{"app": "office", "action": "office.run", "input": map[string]any{"message": "x", "files": []string{id}}}); st != 422 {
		t.Fatalf("carol attached bob's file: %d %v", st, out)
	}
	if st, out := e.call("POST", "/v1/workspaces/us-ops/actions/invoke", "bob", map[string]any{"app": "office", "action": "office.run", "input": map[string]any{"message": "x", "files": []string{"not-a-file"}}}); st != 422 {
		t.Fatalf("%d %v", st, out)
	}
	inv := e.must(201, "POST", "/v1/workspaces/us-ops/actions/invoke", "bob", map[string]any{"app": "office", "action": "office.run", "input": map[string]any{"message": "改成第二版", "files": []string{id}}})
	runID := inv["result"].(map[string]any)["id"].(string)
	sent := e.host.find(func(p string) bool { return p == "/v1/agents/office-assistant/runs" })
	if msgs := sent.Body["messages"].([]any); msgs[0].(map[string]any)["content"] != "改成第二版" {
		t.Fatalf("%v", msgs)
	}
	props := sent.Body["forwardedProps"].(map[string]any)
	att := props["idp.host/attachments"].([]any)[0].(map[string]any)
	readTicket, writeTicket := att["ticket"].(string), props["idp.host/delivery"].(map[string]any)["ticket"].(string)
	if att["ref"] != id || att["name"] != "报价单.docx" {
		t.Fatalf("%v", att)
	}

	// the sandbox, with the read ticket, gets the file; with anything else, nothing
	in := e.internal.URL + "/internal/v1/files"
	st, got, h = raw(t, "GET", in+"/content", readTicket, nil)
	if st != 200 || !bytes.Equal(got, quote) || h.Get("Idp-File-Ref") != id || h.Get("Idp-File-Sha256") != hex.EncodeToString(sum[:]) {
		t.Fatalf("%d %v", st, h)
	}
	for name, tok := range map[string]string{"write ticket": writeTicket, "member token": e.tokens["bob"], "none": "", "garbage": "a.b.c"} {
		if st, _, _ := raw(t, "GET", in+"/content", tok, nil); st != 401 {
			t.Errorf("read with %s: %d", name, st)
		}
	}
	// the internal endpoint is not on the public listener (which answers any other path with the shell's page)
	if _, got, _ := raw(t, "GET", e.api.URL+"/internal/v1/files/content", readTicket, nil); bytes.Equal(got, quote) {
		t.Fatal("the internal endpoint answers on the public listener")
	}

	// the sandbox delivers with the write ticket: the file is bob's, from the run
	edited := docxOf(t, "报价：第二版")
	deliver := func(tok, name string, data []byte) (int, map[string]any) {
		st, out, _ := raw(t, "POST", in, tok, data, "Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}), "Content-Type", "application/octet-stream")
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return st, m
	}
	st, d := deliver(writeTicket, "报价单-修订.docx", edited)
	if st != 201 || d["ref"] == nil || d["name"] != "报价单-修订.docx" || d["size"] != float64(len(edited)) {
		t.Fatalf("%d %v", st, d)
	}
	if st, _ := deliver(readTicket, "x.docx", edited); st != 401 {
		t.Fatalf("delivered with a read ticket: %d", st)
	}
	if st, out := deliver(writeTicket, "macro.docm", edited); st != 422 {
		t.Fatalf("a macro file was delivered: %d %v", st, out)
	}
	if st, out := deliver(writeTicket, "big.txt", bytes.Repeat([]byte("a"), 1<<20+1)); st != 413 {
		t.Fatalf("an oversized file was delivered: %d %v", st, out)
	}
	if st, _ := deliver(writeTicket, "second.txt", []byte("two")); st != 201 {
		t.Fatalf("second delivery: %d", st)
	}
	if st, out := deliver(writeTicket, "third.txt", []byte("three")); st != 409 { // the run may deliver 2
		t.Fatalf("a third delivery: %d %v", st, out)
	}
	runFiles := e.must(200, "GET", "/v1/workspaces/us-ops/files?source="+url.QueryEscape("run:"+runID), "bob", nil)["files"].([]any)
	if len(runFiles) != 2 {
		t.Fatalf("%v", runFiles)
	}
	ref := d["ref"].(string)
	if st, got, _ := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+ref+"/content", e.tokens["bob"], nil); st != 200 || !bytes.Equal(got, edited) {
		t.Fatalf("bob cannot download what the run delivered: %d", st)
	}
	if st, _, _ := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+ref+"/content", e.tokens["carol"], nil); st != 404 {
		t.Fatalf("carol downloaded bob's deliverable: %d", st)
	}
	// an expired ticket no longer works
	e.tickets.Now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	old, _ := e.tickets.Read(e.tenantA, "bob", "bob", id, runID)
	e.tickets.Now = nil
	if st, _, _ := raw(t, "GET", in+"/content", old, nil); st != 401 {
		t.Fatalf("an expired ticket read: %d", st)
	}
	// quota: 6 files of the owner in the tenant (3 so far)
	for i := 0; i < 3; i++ {
		if st, _ := upload("bob", "n.txt", []byte("n")); st != 201 {
			t.Fatalf("upload %d: %d", i, st)
		}
	}
	if st, out := upload("bob", "n.txt", []byte("n")); st != 409 {
		t.Fatalf("the quota did not hold: %d %v", st, out)
	}
	// deleting: the owner only; the read ticket then finds nothing
	e.must(404, "DELETE", "/v1/workspaces/us-ops/files/"+id, "carol", nil)
	e.must(204, "DELETE", "/v1/workspaces/us-ops/files/"+id, "bob", nil)
	if st, _, _ := raw(t, "GET", in+"/content", readTicket, nil); st != 404 {
		t.Fatalf("a deleted file was read: %d", st)
	}
	e.must(204, "DELETE", "/api/v1/workspaces/us-ops/installations/office", "alice", nil)
}
