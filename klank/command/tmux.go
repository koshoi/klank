package command

import (
	"os"
	"os/exec"
	"strings"

	"klank/cli"
)

// ---- tmux integration ----
//
// Every agent lives inside a tmux pane; klank records where:
//
//   - started inside tmux: the container is attached to the invoking pane
//     (docker attach) and that pane's session + pane id are saved.
//   - started outside tmux: a detached tmux session named after the agent is
//     created and `docker attach <cid>` runs inside its pane.
//
// The pane id (e.g. "%3") is stored rather than a "session:window.pane"
// triple because it stays valid when the pane is moved between windows (see
// the swap-pane in Attach).

// tmuxIdentity points at one tmux pane.
type tmuxIdentity struct {
	session string // tmux session name (e.g. "agent001")
	pane    string // tmux pane id (e.g. "%3")
}

// inTmux reports whether the klank process itself runs inside a tmux pane.
func inTmux() bool {
	return os.Getenv("TMUX_PANE") != ""
}

// runCapture runs a command, returning trimmed stdout and captured stderr.
func runCapture(argv ...string) (string, string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), strings.TrimSpace(errb.String()), err
}

// tmuxCurrentIdentity resolves the session and pane id of the pane klank
// runs in (requires $TMUX_PANE, set by tmux for pane processes).
func tmuxCurrentIdentity() tmuxIdentity {
	pane := os.Getenv("TMUX_PANE")
	out, errout, err := runCapture("tmux", "display-message", "-p", "-t", pane, "#S")
	if err != nil {
		cli.Fatal("tmux display-message (pane %s): %v%s", pane, err, errout)
	}
	return tmuxIdentity{session: out, pane: pane}
}

// tmuxHasSession reports whether a tmux session with that exact name exists.
func tmuxHasSession(name string) bool {
	_, _, err := runCapture("tmux", "has-session", "-t", "="+name)
	return err == nil
}

// tmuxPaneExists reports whether the pane id is still alive. Note that
// `display-message -t <pane>` is not usable for this: it exits 0 with empty
// output for dead panes, so the check is a membership test over all panes.
func tmuxPaneExists(pane string) bool {
	out, _, err := runCapture("tmux", "list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		return false
	}
	for _, id := range strings.Split(out, "\n") {
		if strings.TrimSpace(id) == pane {
			return true
		}
	}
	return false
}

// tmuxWindowOfPane returns the window id (e.g. "@1") hosting a pane.
func tmuxWindowOfPane(pane string) string {
	out, errout, err := runCapture("tmux", "display-message", "-p", "-t", pane, "#{window_id}")
	if err != nil {
		cli.Fatal("tmux display-message (pane %s): %v%s", pane, err, errout)
	}
	return out
}

// tmuxStartAgentSession creates a detached tmux session whose single pane
// runs `docker attach <cid>` and returns the pane's identity.
func tmuxStartAgentSession(sess, cid string) tmuxIdentity {
	_, errout, err := runCapture("tmux", "new-session", "-d", "-s", sess, "docker attach "+cid)
	if err != nil {
		cli.Fatal("tmux new-session %s: %v%s", sess, err, errout)
	}
	pane, errout, err := runCapture("tmux", "display-message", "-p", "-t", sess, "#{pane_id}")
	if err != nil || pane == "" {
		cli.Fatal("tmux display-message (session %s): %v%s", sess, err, errout)
	}
	return tmuxIdentity{session: sess, pane: pane}
}

// tmuxAttachClient attaches the current terminal to sess and focuses pane.
func tmuxAttachClient(sess, pane string) {
	win := tmuxWindowOfPane(pane)
	attach := exec.Command("tmux", "attach-session", "-t", sess,
		";", "select-window", "-t", win,
		";", "select-pane", "-t", pane)
	attach.Stdin = os.Stdin
	attach.Stdout = os.Stdout
	attach.Stderr = os.Stderr
	if err := attach.Run(); err != nil {
		cli.Fatal("tmux attach-session: %v", err)
	}
}

// tmuxSwapIntoCurrent substitutes the current pane with the agent's pane.
func tmuxSwapIntoCurrent(pane string) {
	cur := os.Getenv("TMUX_PANE")
	if err := exec.Command("tmux", "swap-pane", "-s", pane, "-t", cur).Run(); err != nil {
		cli.Fatal("tmux swap-pane %s <-> %s: %v", pane, cur, err)
	}
}
