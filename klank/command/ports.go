package command

import (
	"database/sql"
	"fmt"

	"klank/cli"
	"klank/storage"
)

// Port implements `klank port`: print the local address of a named
// container port published with `klank new -p name=container-port`.
//
//	klank port <id|name>            list all named ports of an agent
//	klank port <id|name> <name>     print that port's address (127.0.0.1:<host>)
func Port(db *sql.DB, args []string) {
	if len(args) < 1 || len(args) > 2 {
		cli.Fatal("usage: klank port <id|name> [port-name]")
	}
	idOrName := args[0]
	pruneDeadAgents(db)
	_, ports, ok := storage.GetAgentPorts(db, idOrName)
	if !ok {
		cli.Fatal("no such agent: %s", idOrName)
	}
	agent := storage.AgentName(db, idOrName)

	if len(args) == 1 {
		if len(ports) == 0 {
			cli.Fatal("agent %s has no named ports (start with `klank new -p name=container-port` to add some)", agent)
		}
		for _, p := range ports {
			fmt.Printf("%s\t%d\t127.0.0.1:%d\n", p.Name, p.Container, p.Host)
		}
		return
	}

	want := args[1]
	for _, p := range ports {
		if p.Name == want {
			fmt.Printf("127.0.0.1:%d\n", p.Host)
			return
		}
	}
	cli.Fatal("agent %s has no port named %s", agent, want)
}
