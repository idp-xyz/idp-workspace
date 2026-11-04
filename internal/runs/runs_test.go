package runs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func host(t *testing.T, events ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/v1/runs/run_1/events" || r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "no", 403)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, e := range events {
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", i+1, e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRead(t *testing.T) {
	r := Run{RunID: "run_1", Token: "tok"}
	srv := host(t, `{"type":"RUN_STARTED"}`, `{"type":"TEXT_MESSAGE_START","messageId":"m1"}`, `{"type":"TEXT_MESSAGE_CONTENT","messageId":"m1","delta":"Thinking"}`,
		`{"type":"TEXT_MESSAGE_END","messageId":"m1"}`, `{"type":"TEXT_MESSAGE_START","messageId":"m2"}`, `{"type":"TEXT_MESSAGE_CONTENT","messageId":"m2","delta":"Order 1001 "}`,
		`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m2","delta":"waits for customs."}`, `{"type":"TEXT_MESSAGE_END","messageId":"m2"}`, `{"type":"RUN_FINISHED"}`)
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), r)
	if err != nil || out.failed || out.text != "Order 1001 waits for customs." {
		t.Fatalf("%+v %v", out, err)
	}
	failed := host(t, `{"type":"RUN_STARTED"}`, `{"type":"RUN_ERROR","code":"MODEL_UNAVAILABLE","message":"no provider answered"}`)
	out, err = (&Follower{Endpoint: failed.URL}).read(context.Background(), r)
	if err != nil || !out.failed || out.message != "MODEL_UNAVAILABLE no provider answered" {
		t.Fatalf("%+v %v", out, err)
	}
	cut := host(t, `{"type":"RUN_STARTED"}`)
	if _, err := (&Follower{Endpoint: cut.URL}).read(context.Background(), r); err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatal(err)
	}
	if _, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "other"}); err == nil {
		t.Fatal("a refused stream is not an outcome")
	}
}
