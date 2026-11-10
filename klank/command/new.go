package command

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"klank/cli"
	"klank/config"
	"klank/storage"
)

// appendEnvPair appends a KEY=VALUE pair, replacing an earlier pair with the
// same key so later occurrences win (args override file/earlier flags).
func appendEnvPair(pairs []string, pair string) []string {
	if !strings.Contains(pair, "=") {
		cli.Fatal("invalid env pair %q (expected KEY=VALUE)", pair)
	}
	key := pair[:strings.Index(pair, "=")]
	if key == "" {
		cli.Fatal("invalid env pair %q (empty key)", pair)
	}
	for i, p := range pairs {
		if strings.HasPrefix(p, key+"=") {
			pairs[i] = pair
			return pairs
		}
	}
	return append(pairs, pair)
}

// New implements `klank new`: launch an agent container, record it and
// attach to it (directly when inside tmux, via a dedicated tmux session
// otherwise).
func New(db *sql.DB, cfg *config.Config, args []string) {
	name := ""
	imageName := ""
	envPairs := []string{} // ordered, dedup'd on later -e occurrences
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-i", "--image":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a value", a)
			}
			imageName = args[i+1]
			i++
		case "-e", "--env":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a KEY=VALUE pair", a)
			}
			envPairs = appendEnvPair(envPairs, args[i+1])
			i++
		default:
			if name != "" {
				cli.Fatal("unexpected argument: %s", a)
			}
			name = a
		}
	}
	image := imageName
	if image == "" {
		image = cfg.Image // config default (already includes built-in default)
		cli.Debug("using configured default image: %s", image)
	}

	// env vars: config env_file first, then -e pairs (args override file)
	env := map[string]string{}
	if cfg.EnvFile != "" {
		for k, v := range config.ParseEnvFile(cfg.EnvFile) {
			env[k] = v
		}
		cli.Debug("loaded env file %s", cfg.EnvFile)
	}
	for _, p := range envPairs {
		parts := strings.SplitN(p, "=", 2)
		env[parts[0]] = parts[1]
	}
	for k, v := range env {
		cli.Debug("container env: %s=%s", k, v)
	}

	// tmux strategy: when klank itself runs inside a tmux pane, the container
	// is attached to that pane and its identity recorded; otherwise a
	// dedicated tmux session named after the agent hosts the container.
	var startingInTmux bool
	var ident tmuxIdentity
	if inTmux() {
		startingInTmux = true
		ident = tmuxCurrentIdentity()
		cli.Debug("klank is inside tmux: session=%s pane=%s", ident.session, ident.pane)
	} else {
		if _, err := exec.LookPath("tmux"); err != nil {
			cli.Fatal("not running inside tmux and tmux is not in PATH; install tmux or start klank from a tmux session")
		}
		if name == "" {
			name = storage.NextAgentName(db)
			cli.Debug("defaulted agent/tmux session name: %s", name)
		}
		if tmuxHasSession(name) {
			cli.Fatal("tmux session %s already exists", name)
		}
	}

	fmt.Printf("launching container (image: %s)...\n", image)
	uid, gid := currentUserIDs()
	run := exec.Command("docker", "run", "-d", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"-v", fmt.Sprintf("%s:/work", cli.MustCwd()))
	// container env: pass each merged var as its own -e flag
	for k, v := range env {
		run.Args = append(run.Args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	run.Args = append(run.Args, image)

	run.Stdin = os.Stdin
	out, err := run.Output()
	if err != nil {
		cli.Fatal("docker run: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	cid := strings.TrimSpace(string(out))
	cli.Debug("container id: %s", cid)
	imageHash := dockerImageHash(cid)

	if startingInTmux {
		finalName := storage.InsertAgent(db, cid, name, image, imageHash, ident.session, ident.pane)
		fmt.Printf("container saved: %s (agent %s, tmux pane %s in session %s)\n", cid, finalName, ident.pane, ident.session)
		fmt.Println("attaching...")
		dockerAttach(cid)
		return
	}

	// outside tmux: create the agent's tmux session with the container
	// attached inside its pane, record the pane, then attach the terminal.
	finalName := storage.InsertAgent(db, cid, name, image, imageHash, "", "")
	ident = tmuxStartAgentSession(finalName, cid)
	storage.UpdateAgentTmux(db, cid, ident.session, ident.pane)
	fmt.Printf("container saved: %s (agent %s, tmux pane %s in session %s)\n", cid, finalName, ident.pane, ident.session)
	fmt.Printf("attaching to tmux session %s...\n", ident.session)
	tmuxAttachClient(ident.session, ident.pane)
}
