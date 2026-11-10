package command

import (
	"database/sql"

	"klank/cli"
	"klank/storage"
)

// Get implements `klank get`: print stored data for one agent by id or name.
func Get(db *sql.DB, args []string) {
	if len(args) != 1 {
		cli.Fatal("usage: klank get <id|name>")
	}
	result, ok := storage.GetAgent(db, args[0])
	if !ok {
		cli.Fatal("no such agent: %s", args[0])
	}
	cli.PrintResult(result)
}
