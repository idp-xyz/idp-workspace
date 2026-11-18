package integration

import (
	"archive/zip"
	"bytes"
	"context"
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

	"github.com/jackc/pgx/v5"
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
	// the run stays open on the host until the test lets it finish: its tickets are good while it lasts (D27)
	finish := make(chan struct{})
	e.host.mu.Lock()
	e.host.stream = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: {\"type\":\"RUN_STARTED\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return true
		}
		_, _ = io.WriteString(w, "id: 2\ndata: {\"type\":\"RUN_FINISHED\"}\n\n")
		return true
	}
	e.host.mu.Unlock()
	finished := false
	defer func() {
		if !finished {
			close(finish)
		}
		e.host.mu.Lock()
		e.host.stream = nil
		e.host.mu.Unlock()
	}()
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

	// D28: the file is internal unless its owner said otherwise, and a run that reads it delivers internal files
	if att["sensitivity"] != nil {
		t.Fatalf("an internal file's level is not sent: %v", att)
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
	// a read ticket is spent by its first use (D27): whoever presents it again, the sandbox or someone who copied it,
	// gets nothing, and is told so in words the agent can act on
	st, again, _ := raw(t, "GET", in+"/content", readTicket, nil)
	if st != 409 || bytes.Contains(again, quote[:8]) || !strings.Contains(string(again), "TICKET_SPENT") || !strings.Contains(string(again), "already copied") {
		t.Fatalf("the read ticket worked twice: %d %s", st, again)
	}
	// of several presentations at once exactly one gets the file
	fresh, _ := e.tickets.Read(e.tenantA, "bob", "bob", id, runID)
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() { st, _, _ := raw(t, "GET", in+"/content", fresh, nil); codes <- st }()
	}
	served := 0
	for i := 0; i < 8; i++ {
		switch st := <-codes; st {
		case 200:
			served++
		case 409:
		default:
			t.Errorf("a concurrent read: %d", st)
		}
	}
	if served != 1 {
		t.Fatalf("%d of 8 concurrent presentations of one read ticket were served", served)
	}
	// another tenant's ledger is another ledger: the same ticket id there says nothing about this one
	if err := e.store.SpendTicket(context.Background(), e.tenantB, "jti-of-b", runID, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
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
	// the run ends on the host: from then on its tickets are worth nothing, used or not, read or write (D27)
	unused, _ := e.tickets.Read(e.tenantA, "bob", "bob", id, runID)
	close(finish)
	finished = true
	ended := false
	for i := 0; i < 250 && !ended; i++ {
		var n int
		if err := e.store.InTenant(context.Background(), e.tenantA, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM file_runs_ended WHERE run = $1`, runID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		if ended = n == 1; !ended {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ended {
		t.Fatal("the run finished on the host and its tickets were not ended")
	}
	if st, out, _ := raw(t, "GET", in+"/content", unused, nil); st != 401 || bytes.Equal(out, quote) {
		t.Fatalf("a read ticket of an ended run was served: %d", st)
	}
	if st, out := deliver(writeTicket, "late.txt", []byte("late")); st != 401 {
		t.Fatalf("a file was delivered for an ended run: %d %v", st, out)
	}
	// which says nothing about another run's tickets
	other, _ := e.tickets.Read(e.tenantA, "bob", "bob", id, "ws_another_run")
	if st, got, _ := raw(t, "GET", in+"/content", other, nil); st != 200 || !bytes.Equal(got, quote) {
		t.Fatalf("a ticket of a run that has not ended: %d", st)
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
	gone, _ := e.tickets.Read(e.tenantA, "bob", "bob", id, "ws_another_run")
	if st, _, _ := raw(t, "GET", in+"/content", gone, nil); st != 404 {
		t.Fatalf("a deleted file was read: %d", st)
	}
	// what was remembered about tickets goes when they would have expired anyway
	if err := e.store.InTenant(context.Background(), e.tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE file_ticket_uses SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.PurgeExpiredFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = e.store.InTenant(context.Background(), e.tenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM file_ticket_uses`).Scan(&left)
	})
	if left != 0 {
		t.Fatalf("%d spent tickets are remembered past their expiry", left)
	}
	// D28: a member classifies a file when uploading it; idp-host is told, and what a run that read it delivers is
	// classified the same
	st, secret := raw2(t, e, "POST", "/v1/workspaces/us-ops/files?name=plan.txt&sensitivity=confidential", "carol", []byte("the plan"))
	if st != 201 || secret["sensitivity"] != "confidential" {
		t.Fatalf("%d %v", st, secret)
	}
	if st, out := raw2(t, e, "POST", "/v1/workspaces/us-ops/files?name=plan.txt&sensitivity=top-secret", "carol", []byte("x")); st != 422 {
		t.Fatalf("an unknown level: %d %v", st, out)
	}
	working := make(chan struct{}) // carol's run stays open on the host while the test delivers for it
	defer close(working)
	e.host.mu.Lock()
	e.host.stream = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: {\"type\":\"RUN_STARTED\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-working:
		case <-r.Context().Done():
		}
		return true
	}
	e.host.mu.Unlock()
	e.must(201, "POST", "/v1/workspaces/us-ops/actions/invoke", "carol", map[string]any{"app": "office", "action": "office.run", "input": map[string]any{"message": "summarize", "files": []string{secret["id"].(string)}}})
	sent = e.host.find(func(p string) bool { return p == "/v1/agents/office-assistant/runs" })
	props = sent.Body["forwardedProps"].(map[string]any)
	if got := props["idp.host/attachments"].([]any)[0].(map[string]any); got["sensitivity"] != "confidential" || got["name"] != "plan.txt" {
		t.Fatalf("%v", got)
	}
	st, d = deliver(props["idp.host/delivery"].(map[string]any)["ticket"].(string), "summary.txt", []byte("a summary of the plan"))
	if st != 201 {
		t.Fatalf("%d %v", st, d)
	}
	if f := e.must(200, "GET", "/v1/workspaces/us-ops/files/"+d["ref"].(string), "carol", nil); f["sensitivity"] != "confidential" {
		t.Fatalf("what a run that read a confidential file delivered is classified %v", f["sensitivity"])
	}
	e.must(204, "DELETE", "/api/v1/workspaces/us-ops/installations/office", "alice", nil)
}

// raw2 sends bytes as who and decodes the JSON answer.
func raw2(t *testing.T, e *env, method, path, who string, body []byte) (int, map[string]any) {
	t.Helper()
	st, out, _ := raw(t, method, e.api.URL+path, e.tokens[who], body)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return st, m
}
