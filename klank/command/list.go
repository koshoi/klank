package command

import (
	"database/sql"
	"os"
	"os/exec"
	"strings"

	"klank/cli"
	"klank/storage"
)

// List implements `klank list`: silently prune agents whose container is
// no longer present (e.g. a --rm container that exited), then print all
// known agents.
func List(db *sql.DB) {
	storage.PruneAgents(db, func(cid string) bool {
		return dockerAlive(cid) == dockerRunning
	})
	cli.PrintResult(storage.ListAgents(db))
}

// FzfSelectAgent pipes all agent names into fzf and returns the selection.
// Each line is "name\timage" for context; the name is returned.
func FzfSelectAgent(db *sql.DB) string {
	if _, err := exec.LookPath("fzf"); err != nil {
		cli.Fatal("fzf not found in PATH; install it or pass an <id|name> explicitly")
	}
	names := storage.AllNamesForSelect(db)
	if len(names) == 0 {
		return ""
	}
	fzf := exec.Command("fzf", "--header", "select agent to attach to")
	fzf.Stdin = strings.NewReader(strings.Join(names, "\n"))
	fzf.Stderr = os.Stderr
	out, err := fzf.Output()
	if err != nil {
		// user aborted (exit 130) or fzf failed; empty means cancel
		cli.Debug("fzf exited: %v", err)
		return ""
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)[0])
}
