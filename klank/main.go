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
  new | n [-i image] [-n name] [name]
                   launch a new agent container and attach to it
                   (the name defaults to the basename of the current
                   directory; the current directory is mounted at /work,
                   which is refused when it would expose the whole home
                   directory unless --allow-home is given)
  list | ls        list known agents
  get <id|name>    print stored data for an agent by id or name
  attach | a [<id|name>]
                   bring a running agent's pane back to the foreground
                   (without an identifier, pick via fzf)
  bash | b <id|name>
                   open an interactive bash shell in a running agent
                   container (starts in /work; extra args are passed
                   to bash, e.g. klank bash agent001 -c 'ls -la')
  debug | d        internal database inspection tools
    query <sql>    run a read-only SQL query
    data           show table schemas
  send | s [-w] [--timeout s] <id|name> <text...>
                   send a message to a running agent's pi session
                   (shows up in its TUI as a typed user message;
                   without text arguments, the message is read
                   from stdin)
  status <id|name> show an agent's live state (busy/idle, model,
                   session, context usage) via its control server
  curl <id|name> [curl-args...]
                   curl an agent's control server without looking up
                   its port: the first path-like argument (starting
                   with "/" or http(s)://) is rewritten to the
                   agent's URL, all other arguments pass through to
                   curl verbatim; curl's exit status is preserved
  rename | mv <id|name> <newname>
                   rename an agent (a recorded tmux session name
                   matching the old name is updated in the database;
                   the tmux session itself is left untouched)
  port | ports <id|name> [<name>]
                   print the local address of a named container port
                   published with new -p: with a port name, only that
                   address (e.g. 127.0.0.1:81); without one, all of
                   them as name/port/address lines
  config [-f]       copy the current config (the user config,
                   falling back to the system config; a commented
                   starter template when neither exists) to
                   ./.klank.toml for per-project editing
                   (-f overwrites an existing file)

flags:
  -h               show help
  -d               debug output
  -o <format>      output format: text (default) or json
  -j               shorthand for -o json

new flags:
  -n, --name name  agent name (defaults to the current directory's
                   basename)
      --allow-home allow mounting a directory that contains the
                   entire home directory (e.g. $HOME itself or /)
      --ipv6       run the container on an IPv6-enabled docker network
                   (created on demand as "klank-ipv6"; outbound IPv6
                   needs the host to have IPv6 connectivity)
                   [opt-in; enable via ipv6 = true in config.toml]
      --no-ipv6    use the default (IPv4-only) bridge instead
  -i, --image      container image
  -e, --env KEY=VALUE
                   environment variable passed to the container;
                   repeatable
  -p, --port name=container-port
                   publish a named container port on the local
                   machine; repeatable (e.g. -p http=80 -p grpc=8000).
                   Ports from the config's [ports] section are
                   published automatically; -p overrides them by name.
                   The local port is picked free and close to the
                   container port; mappings are saved in the agent
                   database and in the container's /ports file
  -f, --file FILE  stage the file in a scratch directory under /tmp
                   (named after the first file) and mount that directory
                   at /work instead of the current one: the agent works
                   on just the given files in isolation; repeatable
                   (e.g. -f main.go -f util.go). The scratch dir is
                   removed when the agent's container exits; the default
                   agent name is the first file's name

send flags:
  -w, --wait       wait until the agent settles after the message
      --timeout s  wait timeout in seconds (default 900)

examples:
  klank new
  klank new node:24-bookworm-slim
  klank new -n worker
  klank new -e FOO=bar -e BAZ=qux --allow-home
  klank new --ipv6
  klank new -p http=80 -p http2=8080 -p grpc=8000
  klank new -f main.go -f go.mod
  klank port worker http
  klank curl agent001 /status
  klank curl agent001 /send -d 'test'
  klank -j list
  klank attach agent001
  klank bash agent001
  klank send agent001 fix the failing test in foo.go
  echo "multi-line\nmessage" | klank send agent001
  klank send -w agent001 run the test suite
  klank status agent001
  klank rename agent001 worker
  klank config
`

func main() {
	// command shorthands
	aliases := map[string]string{
		"n":  "new",
		"a":  "attach",
		"b":  "bash",
		"ls": "list",
		"d":  "debug",
		"s":  "send",
		"mv": "rename",
		"ports": "port",
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
	case "bash":
		command.Bash(db, rest)
	case "send":
		command.Send(db, rest)
	case "status":
		command.Status(db, rest)
	case "curl":
		command.Curl(db, rest)
	case "rename":
		command.Rename(db, rest)
	case "config":
		command.Config(rest)
	case "port":
		command.Port(db, rest)
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
