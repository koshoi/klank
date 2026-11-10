package command

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"

	"klank/cli"
	"klank/storage"
)

// Bash implements `klank bash`: open an interactive shell in a running
// agent container:
//
//	klank bash <id|name>
//	klank bash            (pick via fzf)
//
// The shell starts in /work (the mounted project directory) and runs as
// the same user as the container's main process, so file permissions and
// the environment match what the agent sees. Extra arguments are passed
// through to bash (e.g. `klank bash agent001 -c 'ls -la'`); the shell's
// exit status is propagated.
func Bash(db *sql.DB, args []string) {
	pruneDeadAgents(db)
	var cid string
	if len(args) >= 1 {
		var ok bool
		cid, _, _, ok = storage.GetAgentTmux(db, args[0])
		if !ok {
			cli.Fatal("no such agent: %s", args[0])
		}
		args = args[1:]
	} else {
		name := FzfSelectAgent(db)
		if name == "" {
			fmt.Println("no agent selected")
			return
		}
		var ok bool
		cid, _, _, ok = storage.GetAgentTmux(db, name)
		if !ok {
			cli.Fatal("no such agent: %s", name)
		}
	}
	if !dockerIsRunning(cid) {
		cli.Fatal("container %s is not running", shortCid(cid))
	}
	cli.Debug("bash in container %s: %v", shortCid(cid), args)

	execArgs := []string{"exec", "-it", cid, "bash"}
	execArgs = append(execArgs, args...) // extra args go to bash (e.g. -c 'cmd')
	cmd := exec.Command("docker", execArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		cli.Fatal("docker exec: %v", err)
	}
}
