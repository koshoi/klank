// consumer.go - klankd: SSE consumer for agent outbox messages.
//
// Every klank agent runs a control server inside its container
// (klank/bridge.ts) on a loopback port recorded in the agents.port DB
// column. The bridge exposes an agent->host push channel:
//
//	POST /outbox   the agent enqueues typed messages
//	GET  /events   SSE stream (replays unacked on connect, heartbeat pings)
//	POST /ack      consumer acknowledges handled message ids
//	POST /inbox    consumer delivers a result for a message
//
// consumerSet keeps one SSE consumer goroutine per running agent,
// reconciled with the database on every poll tick: new agents get a
// consumer, vanished agents lose theirs. Each message is dispatched by
// type to a handler; a handler that fails is NOT acked, so the bridge
// redelivers on the next connect. Unknown types are acked and ignored
// (forward compatibility with newer agents).
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// handler processes one outbox message payload. It returns the result to
// deliver back to the agent plus its ok flag, or an error if handling
// failed transiently (the message is left unacked for redelivery).
type handler func(data json.RawMessage) (ok bool, result map[string]any, err error)

// bridgeMsg is one message as streamed by the bridge's GET /events
// (the `data:` payload is the full outbox item).
type bridgeMsg struct {
	ID   string          `json:"id"`
	V    int             `json:"v"`
	Type string          `json:"type"`
	TS   int64           `json:"ts"`
	Data json.RawMessage `json:"data"`
}

// errNoEvents means GET /events returned 404: the agent's bridge
// predates the outbox channel (old image, container not rebuilt). The
// endpoint will not appear until the container is recreated, so the
// consumer stops instead of filling the log with reconnect errors.
var errNoEvents = errors.New("bridge has no /events endpoint")

const (
	requestTimeout = 5 * time.Second // per POST (ack/deliver)
	maxEventLine   = 4 * 1024 * 1024 // mermaid sources can be large
)

type consumerSet struct {
	handlers map[string]handler
	client   *http.Client

	mu       sync.Mutex
	running  map[string]context.CancelFunc // agent name -> consumer cancel
	disabled map[string]int                // agent name -> port with an old (pre-outbox) bridge
}

func newConsumerSet(handlers map[string]handler) *consumerSet {
	return &consumerSet{
		handlers: handlers,
		client:   &http.Client{}, // no global timeout: SSE streams live forever
		running:  map[string]context.CancelFunc{},
		disabled: map[string]int{},
	}
}

// syncDB reconciles running consumers with the agents table: one consumer
// per agent with a control port. Agents removed from the database
// (klank prunes dead ones) have their consumers stopped. Called on every
// poll tick; cheap when nothing changed.
func (cs *consumerSet) syncDB(db *sql.DB) {
	want := map[string]int{}
	rows, err := db.Query(`SELECT name, port FROM agents WHERE port IS NOT NULL AND port > 0`)
	if err != nil {
		log.Printf("query agent control ports: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var port int
		if err := rows.Scan(&name, &port); err != nil {
			log.Printf("scan agent control port: %v", err)
			continue
		}
		want[name] = port
	}
	if err := rows.Err(); err != nil {
		log.Printf("iterate agent control ports: %v", err)
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()
	for name, cancel := range cs.running {
		if _, ok := want[name]; !ok {
			cancel()
			delete(cs.running, name)
			log.Printf("agent %s gone; consumer stopped", name)
		}
	}
	for name, port := range want {
		if _, ok := cs.running[name]; ok {
			continue
		}
		if p, off := cs.disabled[name]; off {
			if p == port {
				continue // bridge still old; nothing to consume
			}
			delete(cs.disabled, name) // port changed: new container, maybe new bridge
		}
		ctx, cancel := context.WithCancel(context.Background())
		cs.running[name] = cancel
		go cs.run(ctx, name, port, fmt.Sprintf("http://127.0.0.1:%d", port))
	}
}

// stop cancels every consumer (daemon shutdown).
func (cs *consumerSet) stop() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for name, cancel := range cs.running {
		cancel()
		delete(cs.running, name)
	}
}

// run streams the agent's /events endpoint, reconnecting with capped
// exponential backoff; returns when ctx is done. On reconnect the bridge
// replays everything unacked, so no messages are lost in between. (The
// bridge also supports ?since=<id> to skip already-seen items; the
// consumer deliberately does not use it — full unacked replay is correct
// and simpler, and ?since remains available to future consumers.)
//
// A 404 from /events means the agent's bridge predates the outbox; the
// consumer disables itself (removed from running, recorded in disabled
// keyed by port) so syncDB neither restarts it nor re-encounters the
// error until the agent's port changes — i.e. its container was
// recreated with an image whose bridge actually serves /events.
func (cs *consumerSet) run(ctx context.Context, agent string, port int, base string) {
	log.Printf("consumer: streaming outbox of agent %s at %s", agent, base)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := cs.stream(ctx, base)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNoEvents) {
			log.Printf("consumer: agent %s: %v (bridge predates the outbox); consumer disabled until the agent's port changes", agent, err)
			cs.mu.Lock()
			cs.disabled[agent] = port
			delete(cs.running, agent)
			cs.mu.Unlock()
			return
		}
		// a connection that lasted a while was healthy; reset the backoff
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		log.Printf("consumer: agent %s stream ended (%v); reconnecting in %s", agent, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// stream connects to /events and dispatches messages until the stream or
// ctx ends. Pure stdlib: line scan over the response body (SSE is line-
// oriented); `id:`/`data:` pairs dispatch, comments and blank lines are
// ignored.
func (cs *consumerSet) stream(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/event-stream")
	resp, err := cs.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return errNoEvents
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET /events: %s", resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxEventLine)
	eventID := "" // id: seen for the current (not yet dispatched) event
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			eventID = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(line[len("data:"):])
			if eventID == "" {
				continue // data without id: nothing to ack, skip
			}
			id := eventID
			eventID = ""
			cs.dispatch(ctx, base, id, []byte(payload))
		default:
			// blank line (end of event we already dispatched), ": ping"
			// heartbeat comment, or a field we don't care about
		}
	}
	return sc.Err() // nil on clean EOF, ctx cancellation surfaces as a read error
}

// dispatch handles one message: run the type's handler, deliver the
// result, then ack. Handler failure leaves the message unacked so the
// bridge redelivers it later.
//
// Ordering note: dispatch is fully synchronous (stream -> dispatch ->
// handler -> deliver -> ack, serially per agent), so a handler that
// stalls blocks heartbeats and acks for that agent's stream. Handlers
// must therefore be fast (Start + two local POSTs); long-running work
// belongs in a goroutine owned by the handler.
//
// Redelivery note: this is at-least-once delivery. If deliver or ack
// fails after the handler already took effect (e.g. the bridge was
// briefly unreachable), the bridge replays the message and the handler
// runs again — for show_diagram that means a second browser tab. That
// is accepted (near-harmless, and hard to avoid without idempotency
// keys spanning process restarts); see diagram.go for the same note.
func (cs *consumerSet) dispatch(ctx context.Context, base, id string, payload []byte) {
	var msg bridgeMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		// our own bridge never sends malformed items; ack anyway so a
		// version skew cannot wedge the stream on one bad message
		log.Printf("consumer: malformed message %s: %v (acking)", id, err)
		cs.ack(ctx, base, id)
		return
	}
	if msg.ID != "" && msg.ID != id {
		log.Printf("consumer: item id %s != event id %s; using event id", msg.ID, id)
	}

	h, known := cs.handlers[msg.Type]
	if !known {
		// unknown type: forward compatibility — ack + ignore
		log.Printf("consumer: ignoring unknown outbox type %q (%s)", msg.Type, id)
		cs.ack(ctx, base, id)
		return
	}

	ok, result, err := h(msg.Data)
	if err != nil {
		log.Printf("consumer: handler %q for %s failed (left unacked): %v", msg.Type, id, err)
		return // redelivery on next connect
	}
	if err := cs.deliver(ctx, base, id, ok, result); err != nil {
		log.Printf("consumer: delivering result for %s failed (left unacked): %v", id, err)
		return
	}
	cs.ack(ctx, base, id)
	// success is otherwise silent; one line per handled message keeps
	// the daemon's activity visible without being noisy
	log.Printf("consumer: handled %s %s (%s ok=%v)", msg.Type, id, msg.ID, ok)
}

// deliver posts a handled message's result back to the bridge
// (POST /inbox), where the agent-side tool polls for it.
func (cs *consumerSet) deliver(ctx context.Context, base, id string, ok bool, result map[string]any) error {
	return cs.post(ctx, base, "/inbox", map[string]any{"replyTo": id, "ok": ok, "data": result})
}

// ack tells the bridge a message was processed (POST /ack).
func (cs *consumerSet) ack(ctx context.Context, base, id string) {
	err := cs.post(ctx, base, "/ack", map[string]any{"ids": []string{id}})
	if err != nil {
		log.Printf("consumer: ack %s failed: %v (bridge will redeliver)", id, err)
	}
}

// post sends one JSON request to the bridge with a short timeout.
func (cs *consumerSet) post(ctx context.Context, base, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := cs.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s", path, resp.Status)
	}
	return nil
}
