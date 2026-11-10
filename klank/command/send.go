package command

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"klank/cli"
	"klank/storage"
)

// inboxDir is the bridge extension's spool directory inside the container
// (see klank/bridge.ts).
const inboxDir = "/tmp/pi-inbox"

// Send implements `klank send`: deliver a message to an agent's running pi
// session by dropping it into the bridge inbox inside the container. The
// bridge extension picks it up and injects it as a user message (it shows
// up in the TUI as if typed there).
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

	cid, _, _, ok := storage.GetAgentTmux(db, idOrName)
	if !ok {
		cli.Fatal("no such agent: %s", idOrName)
	}
	if !dockerIsRunning(cid) {
		cli.Fatal("container %s is not running", shortCid(cid))
	}

	// write to a tmp file, then rename: the bridge's fs.watch sees the
	// rename event only after the content is fully written
	id := fmt.Sprintf("msg-%d", time.Now().UnixNano())
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

	status := "sent"
	if wait {
		status = waitForReceipt(cid, id, time.Duration(timeout)*time.Second)
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
