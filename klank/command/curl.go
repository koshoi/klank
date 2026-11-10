package command

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"klank/cli"
	"klank/storage"
)

// Curl implements `klank curl`: forward curl invocations to an agent's
// control server without having to look up its published port.
//
//	klank curl <id|name> [curl-args...]
//
// The first path-like argument (starting with "/" or "http(s)://") is
// rewritten to the agent's control server URL; every other argument is
// passed to curl verbatim:
//
//	klank curl agent001 /status
//	klank curl agent001 /send -d 'hello'
//	klank curl agent001 -X POST /send -H 'X-Foo: bar' -d 'test'
//
// curl's exit status is propagated, and stdin/stdout/stderr are connected
// so piping and streaming work as expected.
func Curl(db *sql.DB, args []string) {
	if len(args) < 1 {
		cli.Fatal("usage: klank curl <id|name> [curl-args...]")
	}
	idOrName := args[0]
	cid, port, ok := storage.GetAgentPort(db, idOrName)
	if !ok {
		cli.Fatal("no such agent: %s", idOrName)
	}
	if port == 0 {
		cli.Fatal("agent %s has no control server (legacy agent started before ports were published)", idOrName)
	}
	if !dockerIsRunning(cid) {
		cli.Fatal("container %s is not running", shortCid(cid))
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	curlArgs := make([]string, 0, len(args)+1)
	rewroteURL := false
	for _, a := range args[1:] {
		if !rewroteURL && (strings.HasPrefix(a, "/") || strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://")) {
			a = base + a
			rewroteURL = true
		}
		curlArgs = append(curlArgs, a)
	}
	if !rewroteURL {
		cli.Fatal("no endpoint given (expected a path like /status or a full URL)")
	}
	cli.Debug("curl %s", strings.Join(curlArgs, " "))

	cmd := exec.Command("curl", curlArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		cli.Fatal("run curl: %v", err)
	}
}
