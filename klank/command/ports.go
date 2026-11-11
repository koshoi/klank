package command

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"klank/cli"
	"klank/storage"
)

// Port implements `klank port`: print the local address of a named
// container port published with `klank new -p name=container-port`.
//
//	klank port <id|name>            list all named ports of an agent
//	klank port <id|name> <name>     print that port's address (127.0.0.1:<host>)
//
// With -j, the list is a JSON array of objects (name, container, host,
// address) and the single-port form is a JSON string with the address.
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
		if cli.IsJSON() {
			type portEntry struct {
				Name      string `json:"name"`
				Container int    `json:"container"`
				Host      int    `json:"host"`
				Address   string `json:"address"`
			}
			entries := make([]portEntry, 0, len(ports))
			for _, p := range ports {
				entries = append(entries, portEntry{p.Name, p.Container, p.Host, fmt.Sprintf("127.0.0.1:%d", p.Host)})
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(entries); err != nil {
				cli.Fatal("encode json: %v", err)
			}
			return
		}
		for _, p := range ports {
			fmt.Printf("%s\t%d\t127.0.0.1:%d\n", p.Name, p.Container, p.Host)
		}
		return
	}

	want := args[1]
	for _, p := range ports {
		if p.Name == want {
			addr := fmt.Sprintf("127.0.0.1:%d", p.Host)
			if cli.IsJSON() {
				if err := json.NewEncoder(os.Stdout).Encode(addr); err != nil {
					cli.Fatal("encode json: %v", err)
				}
				return
			}
			fmt.Println(addr)
			return
		}
	}
	cli.Fatal("agent %s has no port named %s", agent, want)
}
