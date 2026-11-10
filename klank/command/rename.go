package command

import (
	"database/sql"
	"fmt"
	"strings"

	"klank/cli"
	"klank/storage"
)

// Rename implements `klank rename <id|name> <newname>`: change an agent's
// name in the database. When the agent's recorded tmux session is named
// after it, the recorded session name is updated as well; the tmux session
// itself is left untouched.
func Rename(db *sql.DB, args []string) {
	if len(args) != 2 {
		cli.Fatal("usage: klank rename <id|name> <newname>")
	}
	idOrName, newName := args[0], args[1]
	if newName == "" || strings.ContainsAny(newName, " \t\n") {
		cli.Fatal("invalid name %q (must be non-empty and contain no whitespace)", newName)
	}
	pruneDeadAgents(db)

	oldName := storage.AgentName(db, idOrName)
	if oldName == "" {
		cli.Fatal("no such agent: %s", idOrName)
	}
	cid, sessName, _, ok := storage.GetAgentTmux(db, idOrName)
	if !ok {
		cli.Fatal("no such agent: %s", idOrName)
	}
	cli.Debug("renaming agent %s: %s -> %s", cid, oldName, newName)

	// when the recorded tmux session is named after the agent, keep it in
	// sync in the database (the tmux session itself is not renamed)
	newSession := "" // empty: leave the recorded tmux_session untouched
	if sessName == oldName {
		newSession = newName
	}

	storage.RenameAgent(db, idOrName, newName, cid, newSession)
	if newSession != "" {
		fmt.Printf("renamed agent %s -> %s (recorded tmux session %s -> %s)\n", oldName, newName, sessName, newName)
	} else {
		fmt.Printf("renamed agent %s -> %s\n", oldName, newName)
	}
}
