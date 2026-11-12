package command

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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

// sortedPortNames returns config [ports] names in deterministic
// (alphabetical) order — TOML maps have no stable order.
func sortedPortNames(ports map[string]int) []string {
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// allocateHostPort returns a free loopback port "reasonably close" to want:
// it scans upward from want (up to 1000 ports, skipping already-claimed
// ones) and falls back to a random free port. Ports below 1024 usually fail
// to bind for unprivileged users, which the scan naturally skips. The bind
// is only a probe (closed immediately), so an unlucky race can still lose
// the port to another process between probe and docker run.
func allocateHostPort(want int, claimed map[int]bool) int {
	for p := want; p < want+1000 && p <= 65535; p++ {
		if claimed[p] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			cli.Debug("host port %d unavailable: %v", p, err)
			continue
		}
		l.Close()
		claimed[p] = true
		return p
	}
	// fallback: any free ephemeral port
	p := controlPort()
	claimed[p] = true
	return p
}

// parsePortFlag parses a `-p name=container-port` value, validating the
// port name (single token, no '=' or whitespace) and the port number.
func parsePortFlag(v string) (name string, container int) {
	n, p, ok := strings.Cut(v, "=")
	if !ok || n == "" || p == "" {
		cli.Fatal("invalid port %q (expected name=container-port, e.g. http=80)", v)
	}
	if strings.ContainsAny(n, " \t\n") {
		cli.Fatal("invalid port name %q (no whitespace allowed)", n)
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		cli.Fatal("invalid container port in %q (expected 1-65535)", v)
	}
	return n, port
}

// writePortsFile records the named port mappings in /ports inside the
// container so the agent can discover where its services are published on
// the host (the system-prompt addendum points it at the file). Runs as root
// via docker exec: / is not writable by the container's unprivileged user.
// Best-effort: failure warns but never blocks the launch.
func writePortsFile(cid string, ports []storage.PortMapping) {
	var b strings.Builder
	b.WriteString("# klank named ports: name  container-port  published-address\n")
	for _, p := range ports {
		fmt.Fprintf(&b, "%s\t%d\t127.0.0.1:%d\n", p.Name, p.Container, p.Host)
	}
	x := exec.Command("docker", "exec", "-i", "-u", "root", cid,
		"sh", "-c", "cat > /ports && chmod 644 /ports")
	x.Stdin = strings.NewReader(b.String())
	if out, err := x.CombinedOutput(); err != nil {
		cli.Warn("write /ports in container: %v\n%s", err, strings.TrimSpace(string(out)))
	} else {
		cli.Debug("wrote /ports (%d named ports)", len(ports))
	}
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
func recordAgent(db *sql.DB, cid, name, image, imageHash string, port int, tmuxSession, tmuxPane string, ports []storage.PortMapping) string {
	var portsJSON string
	if len(ports) > 0 {
		b, err := json.Marshal(ports)
		if err == nil {
			portsJSON = string(b)
		} else {
			cli.Debug("marshal named ports: %v", err)
		}
	}
	finalName, err := storage.InsertAgent(db, cid, name, image, imageHash, port, tmuxSession, tmuxPane, portsJSON)
	if err != nil {
		if rmErr := exec.Command("docker", "rm", "-f", cid).Run(); rmErr != nil {
			cli.Debug("docker rm -f %s: %v", cid, rmErr)
		}
		cli.Fatal("save container: %v", err)
	}
	return finalName
}

// reapAfterAttach cleans up the agent when its container ended while klank
// was attached to it. Attach returns both on container exit and on client
// detach (docker detach keys, tmux detach), so the decision is state-based:
// only a container that is no longer running triggers cleanup. With --rm,
// docker removes the container itself; this drops the database record so
// the agent's name and ports are immediately reusable (klank new therefore
// cleans up after itself instead of waiting for a later prune). A broken
// docker daemon (dockerUnknown) keeps the record, like pruneDeadAgents.
func reapAfterAttach(db *sql.DB, cid string) {
	switch dockerAlive(cid) {
	case dockerRunning:
		return // detached by the user; the agent keeps running
	case dockerUnknown:
		cli.Debug("docker unavailable; skipping post-attach cleanup for %s", cid)
	default: // dockerMissing (--rm already removed it) or dockerStopped (exited)
		storage.DeleteAgent(db, cid)
		fmt.Println("container exited; agent removed")
	}
}

// stagedFile is one validated -f input: absolute source path, destination
// basename and the file mode to preserve.
type stagedFile struct {
	src, base string
	mode      os.FileMode
}

// planFiles validates every -f file (exists, regular, distinct basenames)
// without touching the filesystem beyond stat. Splitting validation from
// copying lets `klank new` fail on bad input before creating anything, and
// defer the scratch-dir creation until after all other pre-launch checks
// (tmux presence, name conflicts, env parsing) have passed.
func planFiles(files []string) (plan []stagedFile, stem string) {
	stem = "file"
	base := filepath.Base(files[0])
	if s := strings.TrimSuffix(base, filepath.Ext(base)); s != "" {
		stem = strings.NewReplacer(".", "_", ":", "_").Replace(s)
	}
	seen := map[string]bool{} // detect two -f files with the same basename
	for _, f := range files {
		src, err := filepath.Abs(f)
		if err != nil {
			cli.Fatal("resolve %s: %v", f, err)
		}
		info, err := os.Stat(src)
		if err != nil {
			cli.Fatal("stat %s: %v", f, err)
		}
		if !info.Mode().IsRegular() {
			cli.Fatal("not a regular file: %s", f)
		}
		base := filepath.Base(src)
		if seen[base] {
			cli.Fatal("two files share the name %s; give one a different name", base)
		}
		seen[base] = true
		plan = append(plan, stagedFile{src, base, info.Mode().Perm()})
	}
	return plan, stem
}

// copyStaged creates the scratch directory under /tmp named after the
// first file (klank-<stem>-<random>) and copies the planned files into
// it. It is the backing for `klank new -f`: the container mounts this
// scratch dir at /work instead of the cwd, so the agent works on just
// the given files in an isolated environment.
func copyStaged(plan []stagedFile, stem string) string {
	dir, err := os.MkdirTemp("", "klank-"+stem+"-")
	if err != nil {
		cli.Fatal("create scratch dir: %v", err)
	}
	for _, s := range plan {
		in, err := os.Open(s.src)
		if err != nil {
			cli.Fatal("open %s: %v", s.src, err)
		}
		dst := filepath.Join(dir, s.base)
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, s.mode)
		if err != nil {
			in.Close()
			cli.Fatal("create %s: %v", dst, err)
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			cli.Fatal("copy %s: %v", s.src, err)
		}
		if err := out.Close(); err != nil {
			in.Close()
			cli.Fatal("close %s: %v", dst, err)
		}
		in.Close()
		cli.Debug("staged %s -> %s", s.src, dst)
	}
	return dir
}

// removeStagedFiles cleans up the -f scratch dir after attach: when the
// container has exited (--rm removed it) nothing needs the dir, so it is
// deleted; when the user detached and the agent still runs, deleting the
// dir would pull /work out from under it, so the dir is kept and its path
// reported instead. A no-op for plain `klank new` (empty dir).
func removeStagedFiles(dir, cid string) {
	if dir == "" {
		return
	}
	switch dockerAlive(cid) {
	case dockerRunning:
		fmt.Printf("agent still running; keeping scratch dir %s\n", dir)
	case dockerUnknown:
		cli.Warn("docker unavailable; keeping scratch dir %s", dir)
	default: // dockerMissing (--rm already removed it) or dockerStopped (exited)
		if err := os.RemoveAll(dir); err != nil {
			cli.Warn("remove scratch dir %s: %v", dir, err)
		} else {
			cli.Debug("removed scratch dir %s", dir)
		}
	}
}

// New implements `klank new`: launch an agent container, record it and
// attach to it (directly when inside tmux, via a dedicated tmux session
// otherwise).
func New(db *sql.DB, cfg *config.Config, args []string) {
	pruneDeadAgents(db) // free names/sessions held by dead agents
	name := ""
	imageName := ""
	envPairs := []string{} // ordered, dedup'd on later -e occurrences
	type namedPort struct {
		name      string
		container int
	}
	var portFlags []namedPort // ordered -p name=container-port occurrences
	portNames := map[string]bool{}
	var fileArgs []string // -f files to stage in a scratch dir
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
		case "-f", "--file":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a value", a)
			}
			fileArgs = append(fileArgs, args[i+1])
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
		case "-p", "--port":
			if i+1 >= len(args) {
				cli.Fatal("%s requires a name=container-port pair", a)
			}
			n, container := parsePortFlag(args[i+1])
			if portNames[n] {
				cli.Fatal("port name %q given twice", n)
			}
			portNames[n] = true
			portFlags = append(portFlags, namedPort{n, container})
			i++
		default:
			if name != "" {
				cli.Fatal("unexpected argument: %s (agent name already set)", a)
			}
			name = a
		}
	}
	cwd := cli.MustCwd()
	var filePlan []stagedFile // validated -f inputs (nil for a plain launch)
	var stageDir, stageStem string
	if len(fileArgs) > 0 {
		filePlan, stageStem = planFiles(fileArgs)
		if name == "" {
			name = stageStem
			cli.Debug("defaulted agent name from first -f file: %s", name)
		}
		// with -f the cwd itself is never mounted (only file copies are),
		// so the home-mount guard does not apply
	} else if !allowHome {
		guardHomeMount(cwd)
	} else {
		cli.Debug("--allow-home: skipping home directory mount guard")
	}
	if name == "" && filePlan == nil {
		name = agentNameFromCwd(cwd)
		cli.Debug("defaulted agent name from cwd: %s", name)
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

	// all pre-launch checks passed: stage the -f files into their scratch
	// dir, which the container mounts at /work instead of the cwd
	if filePlan != nil {
		stageDir = copyStaged(filePlan, stageStem)
		cwd = stageDir
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
	// named ports: the config's [ports] section supplies defaults (for
	// ports that are common to every launch in this project); -p flags
	// override them by name and can add ports not mentioned in config.
	// Each is published on the host loopback close to its container port
	// (the control port is claimed first so the two can never collide).
	effectivePorts := portFlags
	for _, cfgName := range sortedPortNames(cfg.Ports) {
		if !portNames[cfgName] {
			effectivePorts = append(effectivePorts, namedPort{cfgName, cfg.Ports[cfgName]})
		}
	}
	claimed := map[int]bool{port: true}
	var namedPorts []storage.PortMapping
	for _, np := range effectivePorts {
		host := allocateHostPort(np.container, claimed)
		namedPorts = append(namedPorts, storage.PortMapping{
			Name: np.name, Container: np.container, Host: host,
		})
		cli.Debug("named port %s: container %d -> host %d", np.name, np.container, host)
	}
	run := exec.Command("docker", "run", "-d", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"-v", fmt.Sprintf("%s:/work", cwd),
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port)) // control server (bridge.ts)
	for _, np := range namedPorts {
		run.Args = append(run.Args,
			"-p", fmt.Sprintf("127.0.0.1:%d:%d", np.Host, np.Container))
	}
	run.Args = append(run.Args,
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
	if len(namedPorts) > 0 {
		writePortsFile(cid, namedPorts)
	}
	for _, np := range namedPorts {
		fmt.Printf("named port %s: container %d -> 127.0.0.1:%d\n", np.Name, np.Container, np.Host)
	}

	if startingInTmux {
		finalName := recordAgent(db, cid, name, image, imageHash, port, ident.session, ident.pane, namedPorts)
		fmt.Printf("container saved: %s (agent %s, control port %d, tmux pane %s in session %s)\n", cid, finalName, port, ident.pane, ident.session)
		fmt.Println("attaching...")
		dockerAttach(cid)
		reapAfterAttach(db, cid)
		removeStagedFiles(stageDir, cid)
		return
	}

	// outside tmux: create the agent's tmux session with the container
	// attached inside its pane, record the pane, then attach the terminal.
	finalName := recordAgent(db, cid, name, image, imageHash, port, "", "", namedPorts)
	ident = tmuxStartAgentSession(finalName, cid)
	storage.UpdateAgentTmux(db, cid, ident.session, ident.pane)
	fmt.Printf("container saved: %s (agent %s, control port %d, tmux pane %s in session %s)\n", cid, finalName, port, ident.pane, ident.session)
	fmt.Printf("attaching to tmux session %s...\n", ident.session)
	tmuxAttachClient(ident.session, ident.pane)
	reapAfterAttach(db, cid)
	removeStagedFiles(stageDir, cid)
}
