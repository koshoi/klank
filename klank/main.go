// Command klank manages isolated AI agent containers (see README.md).
package main

import (
	"fmt"
	"os"

	"klank/cli"
	"klank/command"
	"klank/config"
	"klank/storage"
)

const usage = `klank - agent management cli

usage:
  klank [flags] <command> [args]

commands:
  new | n [-i image] [name]
                   launch a new agent container and attach to it
                   (name defaults to agent + sequential number,
                   e.g. agent001, agent002, ...)
  list | ls        list known agents
  get <id|name>    print stored data for an agent by id or name
  attach | a [<id|name>]
                   bring a running agent's pane back to the foreground
                   (without an identifier, pick via fzf)
  debug | d        internal database inspection tools
    query <sql>    run a read-only SQL query
    data           show table schemas
  send | s [-w] [--timeout s] <id|name> <text...>
                   send a message to a running agent's pi session
                   (shows up in its TUI as a typed user message;
                   without text arguments, the message is read
                   from stdin)
  rename | mv <id|name> <newname>
                   rename an agent (a recorded tmux session name
                   matching the old name is updated in the database;
                   the tmux session itself is left untouched)

flags:
  -h               show help
  -d               debug output
  -o <format>      output format: text (default) or json
  -j               shorthand for -o json

new flags:
  -i, --image      container image
  -e, --env KEY=VALUE
                   environment variable passed to the container;
                   repeatable

send flags:
  -w, --wait       wait until the agent settles after the message
      --timeout s  wait timeout in seconds (default 900)

examples:
  klank new
  klank new node:24-bookworm-slim
  klank new -e FOO=bar -e BAZ=qux
  klank -j list
  klank attach agent001
  klank send agent001 fix the failing test in foo.go
  echo "multi-line\nmessage" | klank send agent001
  klank send -w agent001 run the test suite
  klank rename agent001 worker
`

func main() {
	// command shorthands
	aliases := map[string]string{
		"n":  "new",
		"a":  "attach",
		"ls": "list",
		"d":  "debug",
		"s":  "send",
		"mv": "rename",
	}
	var args []string
	argv := os.Args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-h", "--help":
			fmt.Print(usage)
			return
		case "-d", "--debug":
			cli.DebugMode = true
			cli.Debug("debug enabled")
		case "-j":
			cli.OutputFormat = "json"
			cli.Debug("output format: json")
		case "-o":
			if i+1 >= len(argv) {
				cli.Fatal("-o requires a format (text|json)")
			}
			cli.OutputFormat = argv[i+1]
			if cli.OutputFormat != "text" && cli.OutputFormat != "json" {
				cli.Fatal("unknown output format: %s (expected text or json)", cli.OutputFormat)
			}
			cli.Debug("output format: %s", cli.OutputFormat)
			i++
		default:
			args = append(args, a)
		}
	}

	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(1)
	}
	db := storage.Open()
	defer db.Close()
	cfg := config.Load()
	cli.Debug("config: image=%s", cfg.Image)

	cmd, rest := args[0], args[1:]
	if full, ok := aliases[cmd]; ok {
		cmd = full
	}
	cli.Debug("cmd=%s args=%v", cmd, rest)

	switch cmd {
	case "new":
		command.New(db, cfg, rest)
	case "list":
		command.List(db)
	case "get":
		command.Get(db, rest)
	case "attach":
		command.Attach(db, rest)
	case "send":
		command.Send(db, rest)
	case "rename":
		command.Rename(db, rest)
	case "debug":
		command.Debug(db, rest)
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		fmt.Print(usage)
		os.Exit(1)
	}
}
