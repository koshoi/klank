// diagram.go - klankd: show_diagram handler.
//
// Encodes mermaid source into a mermaid.live editor URL and opens it in
// a browser on the host. The result (URL + outcome) is delivered back to
// the agent via the bridge's /inbox, so the model knows whether the user
// actually got the diagram.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// diagramState is the URL-fragment JSON mermaid.live expects:
// base64url-encoded {"code": "...", "mermaid": {"theme": "..."}}.
type diagramState struct {
	Code    string `json:"code"`
	Mermaid struct {
		Theme string `json:"theme"`
	} `json:"mermaid"`
}

// mermaidLiveURL builds https://mermaid.live/edit#<base64url(state)>.
// RawURLEncoding: no padding, fragment- and URL-safe.
//
// Note on redelivery: this handler runs at-least-once. If deliver (POST
// /inbox) or ack fails after a successful browser open, the bridge
// replays the message on the next connect and the browser tab is opened
// a second time. That is inherent to at-least-once delivery and near-
// harmless for show_diagram (an extra tab), so no de-duplication is
// attempted — see consumer.go's dispatch for the full ordering.
func mermaidLiveURL(mermaid string) string {
	var st diagramState
	st.Code = mermaid
	st.Mermaid.Theme = "default"
	b, err := json.Marshal(st)
	if err != nil {
		// json.Marshal of strings cannot fail; keep a sane fallback anyway
		return "https://mermaid.live/edit"
	}
	return "https://mermaid.live/edit#" + base64.RawURLEncoding.EncodeToString(b)
}

// browserOpeners returns candidate open commands for url, most preferred
// first: the -open-command flag (split on spaces), then firefox — each
// only if its binary exists on the host.
//
// Bare `firefox <url>` opens a tab in the *existing* window; `--new-window`
// is inserted so every diagram lands in its own window.
func browserOpeners(openCmd, url string) [][]string {
	var cmds [][]string
	if fields := strings.Fields(openCmd); len(fields) > 0 {
		cmds = append(cmds, append(fields, url))
	}
	if path, err := exec.LookPath("firefox"); err == nil {
		cmds = append(cmds, []string{path, "--new-window", url})
	}
	return cmds
}

// maxMermaid mirrors the bridge's MAX_MERMAID cap (klank/bridge.ts
// validateOutboxData). The bridge rejects oversized payloads with a 400
// before they ever reach the outbox; this second check keeps the handler
// safe if a future entry point (or a buggy bridge) lets one through.
const maxMermaid = 200 * 1024

// handleShowDiagram is the show_diagram handler factory. The returned
// handler never fails transiently: a malformed payload or an unopenable
// browser is a *result* (ok: false) that is delivered and acked — only
// genuine bugs return an error (which leaves the message unacked for
// redelivery).
func handleShowDiagram(openCmd string) handler {
	return func(data json.RawMessage) (bool, map[string]any, error) {
		var payload struct {
			Title   string `json:"title"`
			Mermaid string `json:"mermaid"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return false, map[string]any{"error": fmt.Sprintf("invalid payload: %v", err)}, nil
		}
		if strings.TrimSpace(payload.Mermaid) == "" {
			return false, map[string]any{"error": "invalid payload: mermaid is empty"}, nil
		}
		if len(payload.Mermaid) > maxMermaid {
			return false, map[string]any{"error": fmt.Sprintf("invalid payload: mermaid is too large (%d bytes; max %d) — simplify or split the diagram", len(payload.Mermaid), maxMermaid)}, nil
		}

		url := mermaidLiveURL(payload.Mermaid)
		openers := browserOpeners(openCmd, url)
		var lastErr error
		for _, argv := range openers {
			cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv[0] is a looked-up binary or an explicit flag
			// detached: Start, not Run — a browser that lingers (or fails
			// to render) must never block the consumer loop. Wait in a
			// goroutine so the child does not linger as a zombie.
			if err := cmd.Start(); err != nil {
				lastErr = err
				continue
			}
			go func(argv0 string) {
				// the child is detached, but its exit is diagnostic: an
				// opener that cannot reach the desktop session (no DISPLAY
				// / DBUS under sudo or SSH) exits nonzero right away
				if err := cmd.Wait(); err != nil {
					log.Printf("show_diagram: opener %s exited: %v (url %s)", argv0, err, url)
				}
			}(argv[0])
			log.Printf("show_diagram: opened %s via %s", url, argv[0])
			if payload.Title != "" {
				return true, map[string]any{"url": url, "title": payload.Title}, nil
			}
			return true, map[string]any{"url": url}, nil
		}
		errText := "could not open browser"
		if lastErr != nil {
			errText = fmt.Sprintf("could not open browser: %v", lastErr)
		}
		return false, map[string]any{"error": errText, "url": url}, nil
	}
}
