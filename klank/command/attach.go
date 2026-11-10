package command

import (
	"database/sql"
	"fmt"
	"os"

	"klank/cli"
	"klank/storage"
)

// Attach implements `klank attach`: bring an agent's tmux pane to the
// foreground:
//   - inside tmux: substitute the current pane with the agent's saved pane
//     (tmux swap-pane).
//   - outside tmux: attach to the agent's tmux session and focus the pane.
//
// Agents without a saved pane fall back to plain docker attach.
// Without an identifier, presents all agent names in fzf for selection.
func Attach(db *sql.DB, args []string) {
	if len(args) > 1 {
		cli.Fatal("usage: klank attach [<id|name>]")
	}
	var cid, sessName, pane string
	if len(args) == 1 {
		var ok bool
		cid, sessName, pane, ok = storage.GetAgentTmux(db, args[0])
		if !ok {
			cli.Fatal("no such agent: %s", args[0])
		}
	} else {
		name := FzfSelectAgent(db)
		if name == "" {
			fmt.Println("no agent selected")
			return
		}
		var ok bool
		cid, sessName, pane, ok = storage.GetAgentTmux(db, name)
		if !ok {
			cli.Fatal("no such agent: %s", name)
		}
	}
	// verify the container is actually running
	if !dockerIsRunning(cid) {
		cli.Fatal("container %s is not running", cid)
	}
	cli.Debug("agent %s: tmux session=%s pane=%s", cid, sessName, pane)

	if sessName == "" || pane == "" {
		cli.Debug("agent has no tmux pane saved; falling back to docker attach")
		dockerAttach(cid)
		return
	}
	if !tmuxPaneExists(pane) {
		cli.Fatal("tmux pane %s (session %s) no longer exists", pane, sessName)
	}

	if inTmux() {
		if pane == os.Getenv("TMUX_PANE") {
			fmt.Println("agent is already running on this pane")
			return
		}
		tmuxSwapIntoCurrent(pane)
		fmt.Printf("agent pane %s (session %s) swapped into the current window\n", pane, sessName)
		return
	}
	if !tmuxHasSession(sessName) {
		cli.Fatal("tmux session %s no longer exists", sessName)
	}
	fmt.Printf("attaching to tmux session %s...\n", sessName)
	tmuxAttachClient(sessName, pane)
}
