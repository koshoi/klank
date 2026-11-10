package command

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"klank/cli"
	"klank/config"
	"klank/storage"
)

// controlPort picks a free loopback port on the host for the agent's
// control server (see klank/bridge.ts). Published with -p 127.0.0.1:<p>:<p>
// so only local processes can reach the agent.
func controlPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cli.Fatal("allocate control port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// agentNameFromCwd derives an agent name from a directory path: the
// basename with '.' and ':' replaced (tmux rejects those in session names).
func agentNameFromCwd(dir string) string {
	base := filepath.Base(dir)
	return strings.NewReplacer(".", "_", ":", "_").Replace(base)
}

// guardHomeMount refuses to bind-mount the current directory when doing so
// would expose the user's entire home directory (or more) to the container:
// the cwd is $HOME, an ancestor of it, or the filesystem root. --allow-home
// bypasses this guard.
func guardHomeMount(cwd string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		cli.Debug("home dir unknown, skipping home mount guard")
		return
	}
	home = filepath.Clean(home)
	if cwd == "/" || cwd == home || strings.HasPrefix(home, cwd+"/") {
		cli.Fatal("refusing to mount %s: it contains your whole home directory (%s); pass --allow-home to mount it anyway", cwd, home)
	}
}

// ipv6NetworkName / ipv6Subnet: the shared user-defined docker network
// agents join with --ipv6 (created on demand). The subnet is an arbitrary
// ULA ("klank" spelled in hex nibbles). NAT66/ip6tables is automatic on
// Docker >= 27; on older daemons enable "ip6tables": true (and, pre-27,
// "experimental": true) in /etc/docker/daemon.json.
const (
	ipv6NetworkName = "klank-ipv6"
	ipv6Subnet      = "fd00:6b6c:616e:6b::/64"
)

// ensureIPv6Network returns the name of the IPv6-enabled docker network,
// creating it if needed. On failure it warns and returns "" so the caller
// can fall back to the default bridge: IPv6 is best-effort and must never
// block an agent launch.
func ensureIPv6Network() string {
	out, err := exec.Command("docker", "network", "inspect",
		"--format", "{{.EnableIPv6}}", ipv6NetworkName).Output()
	if err == nil {
		if strings.TrimSpace(string(out)) != "true" {
			cli.Warn("docker network %s exists but is not IPv6-enabled; using the default bridge (remove the network to let klank recreate it)", ipv6NetworkName)
			return ""
		}
		cli.Debug("using existing ipv6 network %s", ipv6NetworkName)
		return ipv6NetworkName
	}
	create := exec.Command("docker", "network", "create",
		"--ipv6", "--subnet", ipv6Subnet, ipv6NetworkName)
	if out, err := create.CombinedOutput(); err != nil {
		cli.Warn("create ipv6 network: %v\n%s\nfalling back to the default bridge (outbound IPv6 disabled)", err, strings.TrimSpace(string(out)))
		return ""
	}
	cli.Debug("created ipv6 network %s (subnet %s)", ipv6NetworkName, ipv6Subnet)
	return ipv6NetworkName
}

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

// recordAgent saves a just-started container in the database; on failure the
// container is removed so a failed save never leaves an orphan running.
func recordAgent(db *sql.DB, cid, name, image, imageHash string, port int, tmuxSession, tmuxPane string) string {
	finalName, err := storage.InsertAgent(db, cid, name, image, imageHash, port, tmuxSession, tmuxPane)
	if err != nil {
		if rmErr := exec.Command("docker", "rm", "-f", cid).Run(); rmErr != nil {
			cli.Debug("docker rm -f %s: %v", cid, rmErr)
		}
		cli.Fatal("save container: %v", err)
	}
	return finalName
}

// New implements `klank new`: launch an agent container, record it and
// attach to it (directly when inside tmux, via a dedicated tmux session
// otherwise).
func New(db *sql.DB, cfg *config.Config, args []string) {
	pruneDeadAgents(db) // free names/sessions held by dead agents
	name := ""
	imageName := ""
	envPairs := []string{} // ordered, dedup'd on later -e occurrences
	allowHome := false
	useIPv6 := cfg.UseIPv6() // opt-in default (config: ipv6 = true)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-i", "--image":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a value", a)
			}
			imageName = args[i+1]
			i++
		case "-n", "--name":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a value", a)
			}
			if name != "" {
				cli.Fatal("agent name given twice (%s and an earlier name)", a)
			}
			name = args[i+1]
			i++
		case "--allow-home":
			allowHome = true
		case "--ipv6":
			useIPv6 = true
		case "--no-ipv6":
			useIPv6 = false
		case "-e", "--env":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a KEY=VALUE pair", a)
			}
			envPairs = appendEnvPair(envPairs, args[i+1])
			i++
		default:
			if name != "" {
				cli.Fatal("unexpected argument: %s (agent name already set)", a)
			}
			name = a
		}
	}
	cwd := cli.MustCwd()
	if name == "" {
		name = agentNameFromCwd(cwd)
		cli.Debug("defaulted agent name from cwd: %s", name)
	}
	if !allowHome {
		guardHomeMount(cwd)
	} else {
		cli.Debug("--allow-home: skipping home directory mount guard")
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

	// secrets staging: each env var is written as a file in a private
	// /tmp dir on the host and bind-mounted at /secrets. The container
	// wrapper exports each file as an env var and deletes it immediately
	// after reading, so secrets never outlive first use (and never show
	// up in docker inspect or process listings).
	var secretsDir string
	if len(env) > 0 {
		d, err := os.MkdirTemp("/tmp", "klank-secrets-")
		if err != nil {
			cli.Fatal("create secrets dir: %v", err)
		}
		secretsDir = d
		defer os.RemoveAll(secretsDir) // no-op once the container emptied it
		if err := os.Chmod(secretsDir, 0o700); err != nil {
			cli.Fatal("chmod secrets dir: %v", err)
		}
		for k, v := range env {
			if err := os.WriteFile(filepath.Join(secretsDir, k), []byte(v), 0o600); err != nil {
				cli.Fatal("write secret %s: %v", k, err)
			}
			cli.Debug("secret staged: %s", k)
		}
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
	var netArgs []string
	if useIPv6 {
		if net := ensureIPv6Network(); net != "" {
			netArgs = []string{"--network", net}
		}
	} else {
		cli.Debug("ipv6 networking disabled; using the default bridge")
	}
	uid, gid := currentUserIDs()
	port := controlPort()
	run := exec.Command("docker", "run", "-d", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"-v", fmt.Sprintf("%s:/work", cwd),
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port), // control server (bridge.ts)
		"-e", fmt.Sprintf("KLANK_PORT=%d", port))
	if name != "" {
		// name the container after the agent so `docker ps` matches klank's view
		run.Args = append(run.Args, "--name", name)
	}
	run.Args = append(run.Args, netArgs...)
	if secretsDir != "" {
		// mount the staged secrets and wrap the image's entrypoint/cmd so
		// vars are exported and their files removed before the agent starts
		run.Args = append(run.Args, "-v", secretsDir+":/secrets")
		entrypoint, cmd := dockerImageCommand(image)
		run.Args = append(run.Args, "--entrypoint", "/bin/sh",
			image, "-c", secretsWrapper(entrypoint, cmd))
	} else {
		run.Args = append(run.Args, image)
	}

	run.Stdin = os.Stdin
	out, err := run.Output()
	if err != nil {
		cli.Fatal("docker run: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	cid := strings.TrimSpace(string(out))
	cli.Debug("container id: %s", cid)
	imageHash := dockerImageHash(cid)

	if startingInTmux {
		finalName := recordAgent(db, cid, name, image, imageHash, port, ident.session, ident.pane)
		fmt.Printf("container saved: %s (agent %s, control port %d, tmux pane %s in session %s)\n", cid, finalName, port, ident.pane, ident.session)
		fmt.Println("attaching...")
		dockerAttach(cid)
		return
	}

	// outside tmux: create the agent's tmux session with the container
	// attached inside its pane, record the pane, then attach the terminal.
	finalName := recordAgent(db, cid, name, image, imageHash, port, "", "")
	ident = tmuxStartAgentSession(finalName, cid)
	storage.UpdateAgentTmux(db, cid, ident.session, ident.pane)
	fmt.Printf("container saved: %s (agent %s, control port %d, tmux pane %s in session %s)\n", cid, finalName, port, ident.pane, ident.session)
	fmt.Printf("attaching to tmux session %s...\n", ident.session)
	tmuxAttachClient(ident.session, ident.pane)
}
