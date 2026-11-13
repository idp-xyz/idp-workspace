package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/transport"
)

// The relay to idp-host (D22) without the database: what reaches idp-host, what comes back, and how a
// refusal is told.

func hostDouble(t *testing.T, handler http.HandlerFunc) *dispatch.Downstream {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &dispatch.Downstream{Endpoint: srv.URL, Auth: dispatch.AuthForward, HTTP: srv.Client()}
}

func TestRelayJSONPassesTheAnswerAndTheCredential(t *testing.T) {
	var got *http.Request
	var body []byte
	ds := hostDouble(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"toolCallId":"call_1","state":"CONFIRMED"}`)
	})
	p := authorization.Principal{Subject: "bob", Tenant: "t", Token: "bob-token"}
	status, out, err := relayJSON(context.Background(), ds, p, http.MethodPost, "/v1/runs/ws_1/tool-calls/call_1/decide", map[string]string{"decision": "confirm"})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusAccepted || string(out) != `{"toolCallId":"call_1","state":"CONFIRMED"}` {
		t.Fatalf("%d %s", status, out)
	}
	if got.Header.Get("Authorization") != "Bearer bob-token" || got.URL.Path != "/v1/runs/ws_1/tool-calls/call_1/decide" || strings.TrimSpace(string(body)) != `{"decision":"confirm"}` {
		t.Fatalf("idp-host saw %s %s %q %q", got.Method, got.URL.Path, got.Header.Get("Authorization"), body)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", got.Header.Get("Content-Type"))
	}
}

func TestRelayJSONTellsTheRefusalWithoutTheAddress(t *testing.T) {
	cases := []struct {
		status  int
		answer  string
		want    int
		code    string
		message string
	}{
		{404, `{"error":{"code":"NOT_FOUND","message":"run not found"}}`, 404, "NOT_FOUND", "run not found"},
		{409, `{"code":"CONFLICT","message":"the call is not pending"}`, 409, "CONFLICT", "the call is not pending"},
		{422, `not json`, 422, "HTTP_422", "idp-host refused the request"},
		{401, `{"error":{"code":"UNAUTHENTICATED","message":"bad token"}}`, 403, "FORBIDDEN", "credential"},
		{503, `{"error":{"code":"INTERNAL","message":"db down"}}`, 502, "TARGET_UNAVAILABLE", "idp-host failed (503)"},
	}
	for _, c := range cases {
		ds := hostDouble(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.answer)
		})
		_, _, err := relayJSON(context.Background(), ds, authorization.Principal{Token: "x"}, http.MethodGet, "/v1/runs/ws_1", nil)
		var pb *transport.Problem
		if err == nil || !asProblem(err, &pb) {
			t.Fatalf("%d: want a problem, got %v", c.status, err)
		}
		if pb.Status != c.want || pb.Code != c.code || !strings.Contains(pb.Message, c.message) || strings.Contains(pb.Message, ds.Endpoint) {
			t.Fatalf("%d: got %d %s %q", c.status, pb.Status, pb.Code, pb.Message)
		}
	}
}

func TestRelayJSONWithoutACredential(t *testing.T) {
	ds := &dispatch.Downstream{Endpoint: "http://127.0.0.1:9", Auth: dispatch.AuthForward}
	_, _, err := relayJSON(context.Background(), ds, authorization.Principal{}, http.MethodGet, "/v1/runs/ws_1", nil)
	var pb *transport.Problem
	if !asProblem(err, &pb) || pb.Code != "TARGET_UNAVAILABLE" {
		t.Fatalf("got %v", err)
	}
}

func TestRelayStreamCopiesEventsAsTheyCome(t *testing.T) {
	gate := make(chan struct{})
	var seen *http.Request
	ds := hostDouble(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "id: 4\ndata: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"delta\":\"Hel\"}\n\n")
		fl.Flush()
		<-gate // the second event only after the first one was read downstream
		_, _ = io.WriteString(w, ": keep-alive\nid: 5\ndata: {\"type\":\"RUN_FINISHED\"}\n\n")
		fl.Flush()
	})
	p := authorization.Principal{Subject: "bob", Token: "bob-token"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayStream(w, r, ds, p, "/v1/runs/ws_1/events")
	}))
	t.Cleanup(srv.Close)
	var release sync.Once
	open := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(open) // runs before the servers close: a failed check must not leave the double waiting (and the test hanging)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/workspaces/us-ops/runs/ws_1/events?after=3", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// the first event is readable before idp-host sends the second: nothing is buffered until the end
	buf := make([]byte, 4096)
	n, err := readSome(resp.Body, buf, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first := string(buf[:n])
	if !strings.Contains(first, "id: 4\n") || !strings.Contains(first, `"delta":"Hel"`) || strings.Contains(first, "RUN_FINISHED") {
		t.Fatalf("first chunk: %q", first)
	}
	open()
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), ": keep-alive\n") || !strings.Contains(string(rest), "id: 5\ndata: {\"type\":\"RUN_FINISHED\"}\n\n") {
		t.Fatalf("rest: %q", rest)
	}
	if seen.URL.Query().Get("after") != "3" || seen.Header.Get("Last-Event-ID") != "3" || seen.Header.Get("Authorization") != "Bearer bob-token" || seen.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("idp-host saw %s %v %q", seen.URL.String(), seen.Header.Get("Last-Event-ID"), seen.Header.Get("Authorization"))
	}
}

func TestRelayStreamTellsARefusal(t *testing.T) {
	ds := hostDouble(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"run not found"}}`)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/us-ops/runs/ws_1/events", nil)
	relayStream(rec, req, ds, authorization.Principal{Token: "x"}, "/v1/runs/ws_1/events")
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 404 || body.Error.Code != "NOT_FOUND" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHostIDs(t *testing.T) {
	for _, ok := range []string{"ws_6f37489e903246baad411a968ad0dcea", "call_1", "thr-1.2:x"} {
		if !hostID.MatchString(ok) {
			t.Fatalf("%q should pass", ok)
		}
	}
	for _, bad := range []string{"", "a/b", "a b", "../x", strings.Repeat("a", 129)} {
		if hostID.MatchString(bad) {
			t.Fatalf("%q should not pass", bad)
		}
	}
}

func asProblem(err error, pb **transport.Problem) bool {
	p, ok := err.(*transport.Problem)
	if ok {
		*pb = p
	}
	return ok
}

// readSome reads what is available within d: a stream that only completes at its end would block here.
// readSome reads until buf holds one whole event (a blank line ends it) or d passes: a single Read may return part of
// what the relay flushed.
func readSome(r io.Reader, buf []byte, d time.Duration) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n := 0
		for n < len(buf) && !bytes.Contains(buf[:n], []byte("\n\n")) {
			m, err := r.Read(buf[n:])
			n += m
			if err != nil {
				ch <- result{n, err}
				return
			}
		}
		ch <- result{n, nil}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(d):
		return 0, fmt.Errorf("no whole event readable within %s: the relay buffers the stream", d)
	}
}
