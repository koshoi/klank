// Package storage owns the sqlite database (~/.klank/klank.db) that records
// known agent containers and their tmux panes.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"klank/cli"
)

// PortMapping describes one named container port published to the host's
// loopback (see `klank new -p name=container-port`). Name is the handle
// used by `klank port <agent> <name>`; Container is the port inside the
// container, Host the port published on 127.0.0.1.
type PortMapping struct {
	Name      string `json:"name"`
	Container int    `json:"container"`
	Host      int    `json:"host"`
}

// Open opens (and creates, if needed) the agent database.
func Open() *sql.DB {
	home, err := os.UserHomeDir()
	if err != nil {
		cli.Fatal("cannot determine home dir: %v", err)
	}
	dir := filepath.Join(home, ".klank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		cli.Fatal("cannot create ~/.klank: %v", err)
	}
	path := filepath.Join(dir, "klank.db")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		cli.Fatal("open db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS agents (
		seq      INTEGER PRIMARY KEY AUTOINCREMENT,
		id       TEXT UNIQUE,
		name     TEXT UNIQUE,
		image    TEXT,
		image_hash TEXT,
		tmux_session TEXT,
		tmux_pane    TEXT,
		port         INTEGER,
		created_at INTEGER DEFAULT (unixepoch())
	)`)
	if err != nil {
		cli.Fatal("init schema: %v", err)
	}
	migrate(db)
	cli.Debug("db open: %s", path)
	return db
}

// migrate adds columns introduced after the initial schema so databases
// created by older klank versions keep working.
func migrate(db *sql.DB) {
	rows, err := db.Query(`PRAGMA table_info(agents)`)
	if err != nil {
		cli.Fatal("pragma table_info: %v", err)
	}
	existing := map[string]bool{}
	for rows.Next() {
		// table_info rows: cid, name, type, notnull, dflt_value, pk
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			cli.Fatal("pragma scan: %v", err)
		}
		existing[name] = true
	}
	rows.Close()
	for _, col := range []struct{ name, typ string }{
		{"tmux_session", "TEXT"},
		{"tmux_pane", "TEXT"},
		{"port", "INTEGER DEFAULT 0"},
		{"ports", "TEXT DEFAULT ''"},
	} {
		if existing[col.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN ` + col.name + ` ` + col.typ); err != nil {
			cli.Fatal("migrate (add %s): %v", col.name, err)
		}
		cli.Debug("migrated: added agents.%s", col.name)
	}
}

// InsertAgent saves a container plus its optional tmux identity and returns
// the agent's final name (auto-numbered when name is empty). Auto-numbering
// picks the first free agent%03d name (not max(seq)+1) so renamed agents
// cannot make the insert fail; the single-statement insert means an
// interrupted klank can never leave a placeholder-named row behind.
func InsertAgent(db *sql.DB, cid, name, image, imageHash string, port int, tmuxSession, tmuxPane, ports string) (string, error) {
	if name == "" {
		name = NextAgentName(db)
	}
	if _, err := db.Exec(`INSERT INTO agents (id, name, image, image_hash, port, tmux_session, tmux_pane, ports) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		cid, name, image, imageHash, port, tmuxSession, tmuxPane, ports); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", fmt.Errorf("agent name %q is already in use", name)
		}
		return "", fmt.Errorf("save container: %w", err)
	}
	cli.Debug("saved agent (name=%s image=%s hash=%s port=%d tmux=%s/%s)", name, image, imageHash, port, tmuxSession, tmuxPane)
	return name, nil
}

// ListAgents returns all known agents ordered by seq.
func ListAgents(db *sql.DB) *cli.QueryResult {
	rows, err := db.Query(`SELECT seq, id, coalesce(name,'-') as name, image, coalesce(image_hash,'-') as hash, coalesce(port,0) as port, coalesce(tmux_session,'-') as tmux_session, coalesce(tmux_pane,'-') as tmux_pane FROM agents ORDER BY seq`)
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	defer rows.Close()
	result, err := cli.ScanRows(rows)
	if err != nil {
		cli.Fatal("scan: %v", err)
	}
	return result
}

// GetAgent returns stored data for an agent by id or name.
func GetAgent(db *sql.DB, idOrName string) (*cli.QueryResult, bool) {
	rows, err := db.Query(`SELECT id, image, coalesce(tmux_session,'-') as tmux_session, coalesce(tmux_pane,'-') as tmux_pane, coalesce(ports,'-') as ports FROM agents WHERE name = ? OR id = ?`, idOrName, idOrName)
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	defer rows.Close()
	result, err := cli.ScanRows(rows)
	if err != nil {
		cli.Fatal("scan: %v", err)
	}
	return result, len(result.Rows) > 0
}

// GetAgentTmux resolves an agent by id or name, returning the container id
// plus the saved tmux identity (empty strings when none was recorded).
func GetAgentTmux(db *sql.DB, idOrName string) (cid, tmuxSession, tmuxPane string, ok bool) {
	err := db.QueryRow(`SELECT id, coalesce(tmux_session,''), coalesce(tmux_pane,'') FROM agents WHERE name = ? OR id = ?`, idOrName, idOrName).
		Scan(&cid, &tmuxSession, &tmuxPane)
	if err == sql.ErrNoRows {
		return "", "", "", false
	}
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	return cid, tmuxSession, tmuxPane, true
}

// GetAgentPort resolves an agent by id or name, returning the container id
// plus the port of its control server (0 for agents started before klank
// published ports).
func GetAgentPort(db *sql.DB, idOrName string) (cid string, port int, ok bool) {
	err := db.QueryRow(`SELECT id, coalesce(port, 0) FROM agents WHERE name = ? OR id = ?`, idOrName, idOrName).
		Scan(&cid, &port)
	if err == sql.ErrNoRows {
		return "", 0, false
	}
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	return cid, port, true
}

// GetAgentPorts resolves an agent by id or name, returning its container
// id and named port mappings (nil when none were published).
func GetAgentPorts(db *sql.DB, idOrName string) (cid string, ports []PortMapping, ok bool) {
	var raw string
	err := db.QueryRow(`SELECT id, coalesce(ports,'') FROM agents WHERE name = ? OR id = ?`, idOrName, idOrName).
		Scan(&cid, &raw)
	if err == sql.ErrNoRows {
		return "", nil, false
	}
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	if raw != "" && raw != "-" {
		if err := json.Unmarshal([]byte(raw), &ports); err != nil {
			cli.Fatal("parse named ports for agent %s: %v", idOrName, err)
		}
	}
	return cid, ports, true
}

// AgentName resolves an agent by id or name and returns its stored name
// (empty string when no such agent exists).
func AgentName(db *sql.DB, idOrName string) string {
	var name string
	err := db.QueryRow(`SELECT name FROM agents WHERE name = ? OR id = ?`, idOrName, idOrName).Scan(&name)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	return name
}

// RenameAgent changes an agent's name (addressed by its container id) and,
// when newTmuxSession is non-empty, the tmux session recorded for it (used
// when the agent owns a session named after it). Fails with a friendly
// message when the new name is already taken.
func RenameAgent(db *sql.DB, idOrName, newName, cid, newTmuxSession string) {
	var err error
	if newTmuxSession != "" {
		_, err = db.Exec(`UPDATE agents SET name = ?, tmux_session = ? WHERE id = ?`, newName, newTmuxSession, cid)
	} else {
		_, err = db.Exec(`UPDATE agents SET name = ? WHERE id = ?`, newName, cid)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			cli.Fatal("cannot rename to %s: name already in use", newName)
		}
		cli.Fatal("rename agent: %v", err)
	}
	cli.Debug("renamed agent %s to %s (tmux_session=%q)", idOrName, newName, newTmuxSession)
}

// UpdateAgentTmux backfills the tmux identity of an already saved agent.
func UpdateAgentTmux(db *sql.DB, cid, tmuxSession, tmuxPane string) {
	if _, err := db.Exec(`UPDATE agents SET tmux_session = ?, tmux_pane = ? WHERE id = ?`, tmuxSession, tmuxPane, cid); err != nil {
		cli.Fatal("update tmux identity: %v", err)
	}
}

// PruneAgents removes every agent for which keep reports false and returns
// the number of removed entries.
func PruneAgents(db *sql.DB, keep func(cid string) bool) int {
	rows, err := db.Query(`SELECT id FROM agents`)
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	var remove []string
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			cli.Fatal("scan: %v", err)
		}
		if !keep(cid) {
			remove = append(remove, cid)
		}
	}
	if err := rows.Err(); err != nil {
		cli.Fatal("scan: %v", err)
	}
	rows.Close()
	for _, cid := range remove {
		if _, err := db.Exec(`DELETE FROM agents WHERE id = ?`, cid); err != nil {
			cli.Fatal("delete agent %s: %v", cid, err)
		}
		cli.Debug("pruned dead agent: %s", cid)
	}
	return len(remove)
}

// NextAgentName returns the first free default agent name (agent001, ...);
// matches the auto-numbering in InsertAgent and stays correct across
// renames (a renamed agent's old-style name is not handed out twice).
func NextAgentName(db *sql.DB) string {
	for i := 1; ; i++ {
		name := fmt.Sprintf("agent%03d", i)
		var taken int
		if err := db.QueryRow(`SELECT count(*) FROM agents WHERE name = ?`, name).Scan(&taken); err != nil {
			cli.Fatal("query: %v", err)
		}
		if taken == 0 {
			return name
		}
	}
}

// AllNamesForSelect returns "name\timage" lines for every known agent,
// used by the fzf picker.
func AllNamesForSelect(db *sql.DB) []string {
	rows, err := db.Query(`SELECT coalesce(name, id), image FROM agents ORDER BY seq`)
	if err != nil {
		cli.Fatal("query: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name, image string
		if err := rows.Scan(&name, &image); err != nil {
			cli.Fatal("scan: %v", err)
		}
		names = append(names, fmt.Sprintf("%s\t%s", name, image))
	}
	if err := rows.Err(); err != nil {
		cli.Fatal("scan: %v", err)
	}
	return names
}
