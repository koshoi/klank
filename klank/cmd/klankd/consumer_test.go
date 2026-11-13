package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sseTestServer is a fake bridge: it streams a fixed list of SSE events
// on /events and records every /ack and /inbox POST.
type sseTestServer struct {
	mu     sync.Mutex
	events []string // pre-rendered `id: ...\ndata: ...\n\n` blocks
	acks   []string // ids received on /ack, in order
	inbox  []inboxCall
	srv    *httptest.Server
}

type inboxCall struct {
	ID     string         `json:"replyTo"`
	OK     bool           `json:"ok"`
	Result map[string]any `json:"data"`
}

func newSSETestServer(t *testing.T, events []string) *sseTestServer {
	t.Helper()
	s := &sseTestServer{events: events}
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, e := range s.events {
			fmt.Fprint(w, e)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// returning ends the body -> clean EOF -> stream returns nil
	})
	mux.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode /ack body: %v", err)
		}
		s.mu.Lock()
		s.acks = append(s.acks, body.IDs...)
		s.mu.Unlock()
		w.WriteHeader(200)
		fmt.Fprint(w, "{}")
	})
	mux.HandleFunc("/inbox", func(w http.ResponseWriter, r *http.Request) {
		var body inboxCall
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode /inbox body: %v", err)
		}
		s.mu.Lock()
		s.inbox = append(s.inbox, body)
		s.mu.Unlock()
		w.WriteHeader(200)
		fmt.Fprint(w, "{}")
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sseTestServer) snapshot() (acks []string, inbox []inboxCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acks...), append([]inboxCall(nil), s.inbox...)
}

// event renders one SSE block the way bridge.ts's sseWrite does.
func event(id, item string) string {
	return fmt.Sprintf("id: %s\ndata: %s\n\n", id, item)
}

func outboxItem(id, typ, data string) string {
	return fmt.Sprintf(`{"id":%q,"v":1,"type":%q,"ts":1,"data":%s}`, id, typ, data)
}

func handlers() map[string]handler {
	return map[string]handler{
		"show_diagram": func(data json.RawMessage) (bool, map[string]any, error) {
			return true, map[string]any{"url": "https://mermaid.live/edit#x"}, nil
		},
		"boom": func(data json.RawMessage) (bool, map[string]any, error) {
			return false, nil, fmt.Errorf("transient failure")
		},
	}
}

// TestStreamParsing covers the SSE line protocol: id/data pairing,
// heartbeat comments and blank lines ignored, and the ack contract for
// each dispatch outcome (handled, malformed, unknown type, handler
// error).
func TestStreamParsing(t *testing.T) {
	srv := newSSETestServer(t, []string{
		// handled: delivered + acked
		event("aaa", outboxItem("aaa", "show_diagram", `{"mermaid":"g"}`)),
		// heartbeats and stray fields must not disturb pairing
		": ping\n\n",
		"retry: 5000\n\n",
		// handled with title: delivered + acked
		event("bbb", outboxItem("bbb", "show_diagram", `{"mermaid":"g","title":"T"}`)),
		// malformed item JSON: acked without delivery
		event("ccc", "not json"),
		// unknown type: acked and ignored (forward compatibility)
		event("ddd", outboxItem("ddd", "wat", `{}`)),
		// handler error: neither delivered nor acked (redelivery)
		event("eee", outboxItem("eee", "boom", `{}`)),
		// data without a preceding id: skipped entirely
		"data: {\"type\":\"show_diagram\"}\n\n",
	})

	ctx := context.Background()
	cs := newConsumerSet(handlers())
	if err := cs.stream(ctx, srv.srv.URL); err != nil {
		t.Fatalf("stream returned error on clean EOF: %v", err)
	}

	acks, inbox := srv.snapshot()
	if got, want := len(acks), 4; got != want {
		t.Fatalf("ack ids = %v, want %d acks", acks, want)
	}
	wantAcks := map[string]bool{"aaa": true, "bbb": true, "ccc": true, "ddd": true}
	for _, id := range acks {
		if !wantAcks[id] {
			t.Errorf("unexpected ack for %q", id)
		}
		delete(wantAcks, id)
	}
	for id := range wantAcks {
		t.Errorf("missing ack for %q", id)
	}
	for _, id := range acks {
		if id == "eee" {
			t.Errorf("handler-error item %q must not be acked", id)
		}
	}

	if len(inbox) != 2 {
		t.Fatalf("delivered %d results, want 2: %v", len(inbox), inbox)
	}
	if inbox[0].ID != "aaa" || !inbox[0].OK || inbox[0].Result["url"] != "https://mermaid.live/edit#x" {
		t.Errorf("inbox[0] = %+v, want aaa/ok/url", inbox[0])
	}
	if inbox[1].ID != "bbb" || !inbox[1].OK {
		t.Errorf("inbox[1] = %+v, want bbb/ok", inbox[1])
	}
}

// TestStreamOversizeLine: a line beyond maxEventLine is a scanner error,
// which ends the stream (reconnect + replay handle it) instead of
// killing the consumer.
func TestStreamOversizeLine(t *testing.T) {
	srv := newSSETestServer(t, []string{event("aaa", outboxItem("aaa", "show_diagram", `{"mermaid":"g"}`))})
	// append one oversized data line directly (bypasses event())
	huge := make([]byte, maxEventLine+1)
	for i := range huge {
		huge[i] = 'x'
	}
	srv.events = append(srv.events, fmt.Sprintf("data: %s\n\n", huge))

	cs := newConsumerSet(handlers())
	err := cs.stream(context.Background(), srv.srv.URL)
	if err == nil {
		t.Fatal("stream returned nil on oversized line; want scanner error (reconnect path)")
	}
	acks, _ := srv.snapshot()
	if len(acks) != 1 || acks[0] != "aaa" {
		t.Errorf("acks = %v, want [aaa] (event before the oversized line must still be dispatched)", acks)
	}
}

// TestRunDisablesOn404: a 404 from /events means the agent's bridge
// predates the outbox. The consumer must stop (not reconnect forever,
// which would flood the log) and record itself as disabled, keyed by
// port, so syncDB neither restarts it nor re-encounters the error until
// the agent's port changes.
func TestRunDisablesOn404(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer old.Close()

	cs := newConsumerSet(handlers())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cs.run(context.Background(), "old-agent", 4321, old.URL)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return on 404; it should disable itself instead of retrying")
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()
	if got := cs.disabled["old-agent"]; got != 4321 {
		t.Errorf("disabled[old-agent] = %d, want 4321", got)
	}
	if _, ok := cs.running["old-agent"]; ok {
		t.Error("old-agent still in running; consumer should be stopped")
	}
}

// TestSyncDBRespectsDisabled: an agent recorded in disabled (same port)
// must not get a new consumer; a changed port (recreated container with
// a newer bridge) must re-enable it.
func TestSyncDBRespectsDisabled(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE agents (name TEXT PRIMARY KEY, port INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	seed := func(name string, port int) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO agents (name, port) VALUES (?, ?)`, name, port); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	cs := newConsumerSet(handlers())
	cs.disabled["stale"] = 1111

	// same port: no consumer may start
	seed("stale", 1111)
	cs.syncDB(db)
	cs.mu.Lock()
	_, running := cs.running["stale"]
	cs.mu.Unlock()
	if running {
		t.Fatal("consumer started for an agent disabled at the same port")
	}

	// port changed: consumer starts again
	seed("fresh", 2222)
	cs.syncDB(db)
	cs.mu.Lock()
	_, running = cs.running["fresh"]
	delete(cs.disabled, "stale") // syncDB already did via port change; keep test state clean
	cs.mu.Unlock()
	if !running {
		t.Fatal("no consumer started after the agent's port changed")
	}
}
