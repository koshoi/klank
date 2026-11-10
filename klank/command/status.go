package command

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"klank/cli"
	"klank/storage"
)

// Status implements `klank status`: query an agent's control server for its
// live state (busy/idle, model, session, context usage).
//
//	klank status <id|name>
//
// Agents without a control server (started before ports were published)
// fall back to the container's running state.
func Status(db *sql.DB, args []string) {
	if len(args) != 1 {
		cli.Fatal("usage: klank status <id|name>")
	}
	idOrName := args[0]
	cid, port, ok := storage.GetAgentPort(db, idOrName)
	if !ok {
		cli.Fatal("no such agent: %s", idOrName)
	}
	if !dockerIsRunning(cid) {
		cli.Fatal("container %s is not running", shortCid(cid))
	}

	if port == 0 {
		// legacy agent: no control server to query
		if cli.IsJSON() {
			enc := json.NewEncoder(os.Stdout)
			_ = enc.Encode(map[string]any{
				"agent":     idOrName,
				"container": shortCid(cid),
				"status":    "running (legacy agent: no control server)",
			})
			return
		}
		fmt.Printf("running (legacy agent: no control server)\n")
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if err != nil {
		cli.Fatal("control server on port %d: %v\nhint: the agent's bridge extension must be loaded (rebuild the image and start a new agent)", port, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		cli.Fatal("read control server response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cli.Fatal("bridge /status failed: HTTP %d (%s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if cli.IsJSON() {
		// pretty-print the bridge's own status object verbatim
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			cli.Fatal("decode control server response: %v", err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}

	var s struct {
		Busy  bool  `json:"busy"`
		Idle  *bool `json:"idle"`
		Model *struct {
			ID       interface{} `json:"id"`
			Provider interface{} `json:"provider"`
		} `json:"model"`
		SessionFile  string          `json:"sessionFile"`
		ContextUsage json.RawMessage `json:"contextUsage"`
		UptimeSec    int64           `json:"uptimeSec"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		cli.Fatal("decode control server response: %v", err)
	}
	state := "idle"
	if s.Busy {
		state = "busy"
	}
	model := "-"
	if s.Model != nil && s.Model.ID != nil {
		model = fmt.Sprintf("%v", s.Model.ID)
	}
	fmt.Printf("agent:   %s (%s)\n", idOrName, shortCid(cid))
	fmt.Printf("state:   %s (up %ds)\n", state, s.UptimeSec)
	fmt.Printf("model:   %s\n", model)
	fmt.Printf("session: %s\n", s.SessionFile)
	if len(s.ContextUsage) > 0 && string(s.ContextUsage) != "null" {
		fmt.Printf("usage:   %s\n", s.ContextUsage)
	}
}
