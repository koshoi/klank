package command

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"klank/cli"
	"klank/storage"
)

// inboxDir is the legacy bridge spool directory inside the container (see
// klank/bridge.ts). Used only for agents started before klank published
// control ports (port == 0 in the database).
const inboxDir = "/tmp/pi-inbox"

// Send implements `klank send`: deliver a message to an agent's running pi
// session. New agents expose a control server on a published loopback port
// (bridge.ts); the message is POSTed to /message, optionally long-polled
// via ?wait=1 until the agent settles. It shows up in the TUI as if typed
// there.
//
//	klank send [-w] [--timeout s] <id|name> <text...>
//
// With no text arguments the message is read from stdin (multi-line safe).
func Send(db *sql.DB, args []string) {
	wait := false
	timeout := 900 // seconds
	var idOrName string
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-w", "--wait":
			wait = true
		case "--timeout":
			if i+1 >= len(args) {
				cli.Fatal("--timeout requires a value (seconds)")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &timeout); err != nil {
				cli.Fatal("invalid --timeout value: %s", args[i])
			}
			if timeout <= 0 {
				cli.Fatal("--timeout must be positive")
			}
		default:
			if idOrName == "" {
				idOrName = a
			} else {
				words = append(words, a)
			}
		}
	}
	if idOrName == "" {
		cli.Fatal("usage: klank send [-w] <id|name> <text...>   (or pipe the message via stdin)")
	}

	var message string
	if len(words) > 0 {
		message = strings.Join(words, " ")
	} else {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			cli.Fatal("read stdin: %v", err)
		}
		message = string(data)
	}
	message = strings.TrimSpace(message)
	if message == "" {
		cli.Fatal("empty message")
	}

	cid, port, ok := storage.GetAgentPort(db, idOrName)
	if !ok {
		cli.Fatal("no such agent: %s", idOrName)
	}
	if !dockerIsRunning(cid) {
		cli.Fatal("container %s is not running", shortCid(cid))
	}

	var id, status string
	if port > 0 {
		id, status = sendHTTP(port, message, wait, time.Duration(timeout)*time.Second)
	} else {
		id, status = sendSpool(cid, message, wait, time.Duration(timeout)*time.Second)
	}

	if cli.IsJSON() {
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(map[string]string{
			"agent":     idOrName,
			"container": shortCid(cid),
			"message":   id,
			"status":    status,
		})
		return
	}
	fmt.Printf("%s: %s (%s)\n", status, id, idOrName)
}

// bridgeResponse is the JSON returned by the bridge control server.
type bridgeResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// sendHTTP delivers the message via the bridge control server. With wait it
// long-polls (?wait=1): the server holds the response until the agent
// settles, so no client-side polling is needed.
func sendHTTP(port int, message string, wait bool, timeout time.Duration) (id, status string) {
	url := fmt.Sprintf("http://127.0.0.1:%d/message", port)
	if wait {
		url += "?wait=1"
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(url, "text/plain", strings.NewReader(message))
	if err != nil {
		cli.Fatal("control server on port %d: %v\nhint: the agent's bridge extension must be loaded (rebuild the image and start a new agent)", port, err)
	}
	defer resp.Body.Close()
	var br bridgeResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		cli.Fatal("decode control server response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cli.Fatal("bridge rejected the message: %s", br.Error)
	}
	if br.Status == "done" {
		return "msg-http", "done"
	}
	return "msg-http", br.Status
}

// sendSpool is the legacy transport: write the message into the bridge
// inbox inside the container (tmp file + atomic rename so fs.watch sees it
// only when fully written), then poll for the bridge's receipt files.
func sendSpool(cid, message string, wait bool, timeout time.Duration) (id, status string) {
	id = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	p := fmt.Sprintf("%s/%s", inboxDir, id)
	script := fmt.Sprintf(`umask 0000; cat > '%s.tmp' && mv '%s.tmp' '%s'`, p, p, p)
	write := exec.Command("docker", "exec", "-i", cid, "sh", "-c", script)
	write.Stdin = strings.NewReader(message)
	var stderr bytes.Buffer
	write.Stderr = &stderr
	cli.Debug("writing message %s to %s", id, shortCid(cid))
	if err := write.Run(); err != nil {
		cli.Fatal("write to bridge inbox: %v (%s)\nhint: the klank bridge extension must be loaded in the agent's pi session (rebuild the image and start a new agent)", err, strings.TrimSpace(stderr.String()))
	}
	if wait {
		return id, waitForReceipt(cid, id, timeout)
	}
	return id, "sent"
}

// waitForReceipt polls the container until the bridge writes the .done (or
// .error) receipt for the message, or the deadline passes.
func waitForReceipt(cid, id string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	done := fmt.Sprintf("%s/%s.done", inboxDir, id)
	errp := fmt.Sprintf("%s/%s.error", inboxDir, id)
	for {
		out, err := exec.Command("docker", "exec", cid, "sh", "-c",
			fmt.Sprintf(`if [ -e '%s' ]; then echo done; elif [ -e '%s' ]; then echo error; fi`, done, errp)).Output()
		if err == nil {
			switch strings.TrimSpace(string(out)) {
			case "done":
				return "done"
			case "error":
				cli.Fatal("bridge failed to deliver %s (see the agent's stderr in its tmux pane)", id)
			}
		}
		if time.Now().After(deadline) {
			cli.Fatal("timed out waiting for agent to settle after %s", id)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// shortCid truncates a container id for display.
func shortCid(cid string) string {
	if len(cid) > 12 {
		return cid[:12]
	}
	return cid
}
