// klank-proxy-watcher: a standalone daemon that gives every service
// inside a klank container a stable hostname and runs its own caddy.
//
// It polls klank's sqlite database (~/.klank/klank.db), keeps a
// Caddyfile in sync with every named port published via
// `klank new -p name=container-port` (<port-name>.<agent>.<domain> ->
// 127.0.0.1:<host>, e.g. http.myproject.klank.localhost), and serves a
// dashboard at the domain root (klank.localhost). Caddy runs as a
// supervised child process: started by the daemon, restarted if it
// dies, reloaded (SIGUSR1) when the config changes. One systemd unit
// is the entire deployment — no separate caddy service.
//
// Build:  CGO_ENABLED=0 go build -o klank-proxy-watcher .
// Run:    ./klank-proxy-watcher            (as the klank user; see -h)
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite: enables a fully static binary
)

// route is one proxy route: an fqdn mapped to a published host address.
type route struct {
	fqdn     string
	upstream string
	agent    string
	portName string
}

// portMapping mirrors klank/storage.PortMapping — the JSON stored in the
// agents.ports column. Keep in sync with klank: this binary is compiled
// separately, so schema drift fails silently at runtime (see README).
type portMapping struct {
	Name      string `json:"name"`
	Container int    `json:"container"`
	Host      int    `json:"host"`
}

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("klank-proxy-watcher: ")

	interval := flag.Duration("i", 2*time.Second, "poll interval (min 250ms)")
	file := flag.String("f", "", "Caddyfile path; the dashboard (index.html, klank-agents.json) is written next to it (default ~/.klank/Caddyfile)")
	domain := flag.String("d", "", "domain suffix (default $KLANK_PROXY_DOMAIN or \"klank.localhost\"); the dashboard is served at the domain itself")
	caddyBin := flag.String("caddy", "caddy", "path to the caddy binary to supervise")
	dbPath := flag.String("db", "", "klank database path (default ~/.klank/klank.db)")
	once := flag.Bool("once", false, "sync once and exit (no polling, no caddy)")
	flag.Parse()

	if *domain == "" {
		*domain = os.Getenv("KLANK_PROXY_DOMAIN")
	}
	if *domain == "" {
		*domain = "klank.localhost"
	}
	if *interval < 250*time.Millisecond {
		log.Fatalf("interval must be at least 250ms")
	}
	home := homeDir()
	if *file == "" {
		*file = filepath.Join(home, ".klank", "Caddyfile")
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(home, ".klank", "klank.db")
	}
	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("klank database not readable: %v (run as the klank user, or pass -db)", err)
	}

	// Caddy is a hard dependency — it is the thing that serves the
	// routes and the dashboard. Fail fast (systemd retries with backoff).
	if _, err := exec.LookPath(*caddyBin); err != nil {
		log.Fatalf("caddy binary %q not found: %v (install caddy, or pass -caddy with the full path)", *caddyBin, err)
	}

	// Open read-only: this daemon must never write to klank's database.
	// WAL journal mode lets our reads proceed while klank writes.
	dsn := "file:" + *dbPath + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer db.Close()

	// sync regenerates all generated files from the current database
	// state. Returns whether the Caddyfile content changed.
	sync := func() bool {
		routes, err := loadRoutes(db, *domain)
		if err != nil {
			log.Printf("%v", err)
			return false
		}
		dir := filepath.Dir(*file)
		// the Caddyfile's directory may not exist yet (first run);
		// create it or every write below fails and caddy cannot start
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("create %s: %v", dir, err)
			return false
		}

		changed, err := writeIfChanged(*file, renderCaddyfile(routes, *domain, dir))
		if err != nil {
			log.Printf("write %s: %v", *file, err)
			return false
		}
		if changed {
			log.Printf("updated %s (%d route%s)", *file, len(routes), plural(len(routes)))
		}

		// Dashboard files are served straight from disk by caddy's
		// file_server; updating them needs no reload.
		for _, f := range []struct{ path, content string }{
			{filepath.Join(dir, "index.html"), renderDashboard(routes, *domain)}, // named index.html so caddy's file_server serves it at /
			{filepath.Join(dir, "klank-agents.json"), agentsJSON(routes)},
		} {
			if c, err := writeIfChanged(f.path, f.content); err != nil {
				log.Printf("write %s: %v", f.path, err)
			} else if c {
				log.Printf("updated %s", f.path)
			}
		}
		return changed
	}

	if *once {
		sync()
		routes, _ := loadRoutes(db, *domain)
		for _, r := range routes {
			fmt.Printf("%s -> %s\n", r.fqdn, r.upstream)
		}
		return
	}

	log.Printf("polling %s every %s; writing %s; domain .%s; dashboard at %s", *dbPath, *interval, *file, *domain, *domain)

	// First sync before starting caddy so it boots with current config.
	sync()

	caddyExited := make(chan error, 1)
	var caddyProc *os.Process

	// startCaddyAndWait launches the supervised caddy child and wires a
	// waiter goroutine that reports its exit to caddyExited. caddy logs
	// to stderr, which systemd captures in the journal alongside our log.
	startCaddyAndWait := func() {
		cmd := exec.Command(*caddyBin, "run", "--config", *file, "--adapter", "caddyfile")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("cannot start caddy: %v", err)
			caddyProc = nil
			return
		}
		caddyProc = cmd.Process
		log.Printf("caddy started (pid %d, config %s)", cmd.Process.Pid, *file)
		go func() { caddyExited <- cmd.Wait() }()
	}

	startCaddyAndWait()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-caddyExited:
			caddyProc = nil
			log.Printf("caddy exited (%v); restarting in 2s", err)
			time.Sleep(2 * time.Second)
			startCaddyAndWait()
		case <-ticker.C:
			if changed := sync(); changed && caddyProc != nil {
				// caddy re-reads its config file on SIGUSR1; if the
				// process is gone the signal errors and we ignore it —
				// the restart path reads the fresh file anyway.
				if err := caddyProc.Signal(syscall.SIGUSR1); err != nil {
					log.Printf("caddy reload signal: %v (caddy may be restarting)", err)
				}
			}
		case s := <-sig:
			log.Printf("got %v, stopping caddy and exiting", s)
			if caddyProc != nil {
				_ = caddyProc.Signal(syscall.SIGTERM)
				done := false
				for i := 0; i < 50 && !done; i++ { // up to 5s for graceful stop
					select {
					case <-caddyExited:
						done = true
					case <-time.After(100 * time.Millisecond):
					}
				}
				if !done {
					_ = caddyProc.Kill()
				}
			}
			return
		}
	}
}

// loadRoutes reads every agent's named port mappings from the database.
// A bad ports column on one agent is skipped with a warning instead of
// failing the whole sync — the daemon must survive klank schema drift
// (it is compiled separately from klank's storage package; see README).
func loadRoutes(db *sql.DB, domain string) ([]route, error) {
	rows, err := db.Query(`SELECT name, coalesce(ports,'') FROM agents ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query agents: %w", err)
	}
	defer rows.Close()

	var routes []route
	seen := map[string]bool{}
	for rows.Next() {
		var agent, raw string
		if err := rows.Scan(&agent, &raw); err != nil {
			return nil, fmt.Errorf("scan agents: %w", err)
		}
		if raw == "" || raw == "-" {
			continue
		}
		var ports []portMapping
		if err := json.Unmarshal([]byte(raw), &ports); err != nil {
			log.Printf("agent %s: skipping bad ports column %q: %v", agent, raw, err)
			continue
		}
		for _, p := range ports {
			fqdn := fmt.Sprintf("%s.%s.%s", p.Name, agent, domain)
			if seen[fqdn] {
				log.Printf("duplicate route %s (agent %s port %s)", fqdn, agent, p.Name)
				continue
			}
			seen[fqdn] = true
			routes = append(routes, route{
				fqdn:     fqdn,
				upstream: fmt.Sprintf("127.0.0.1:%d", p.Host),
				agent:    agent,
				portName: p.Name,
			})
		}
	}
	return routes, rows.Err()
}

// renderCaddyfile produces the full desired Caddyfile content: one site
// block per route plus the dashboard site (file_server on the domain
// root). The header is static so unchanged mappings produce
// byte-identical files (content comparison decides when to reload).
func renderCaddyfile(routes []route, domain, dashboardRoot string) string {
	var b strings.Builder
	b.WriteString("# managed by klank-proxy-watcher -- DO NOT EDIT\n")
	// explicit http:// scheme: without it caddy treats every site as
	// HTTPS (auto-redirect + local TLS certs), which breaks plain-http
	// *.klank.localhost names that never leave the machine
	b.WriteString(fmt.Sprintf("\nhttp://%s {\n\troot * %s\n\tfile_server\n}\n", domain, dashboardRoot))
	for _, r := range routes {
		b.WriteString(fmt.Sprintf("\nhttp://%s {\n\treverse_proxy %s\n}\n", r.fqdn, r.upstream))
	}
	// no trailing blank line: keeps the output byte-identical to
	// `caddy fmt`, so caddy never logs the "input is not formatted" warning
	return b.String()
}

// groupByAgent groups routes per agent, preserving the sorted-by-agent
// order of the input.
func groupByAgent(routes []route) []struct {
	agent string
	ports []route
} {
	var groups []struct {
		agent string
		ports []route
	}
	idx := map[string]int{}
	for _, r := range routes {
		if i, ok := idx[r.agent]; ok {
			groups[i].ports = append(groups[i].ports, r)
			continue
		}
		idx[r.agent] = len(groups)
		groups = append(groups, struct {
			agent string
			ports []route
		}{r.agent, []route{r}})
	}
	return groups
}

// agentsJSON is the JSON feed the dashboard page polls to refresh
// itself without a reload.
func agentsJSON(routes []route) string {
	type port struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Upstream string `json:"upstream"`
	}
	type agent struct {
		Name  string `json:"name"`
		Ports []port `json:"ports"`
	}
	var out []agent
	for _, g := range groupByAgent(routes) {
		a := agent{Name: g.agent, Ports: []port{}}
		for _, r := range g.ports {
			a.Ports = append(a.Ports, port{r.portName, "http://" + r.fqdn, r.upstream})
		}
		out = append(out, a)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(b)
}

const dashboardStyle = `
	body { font-family: system-ui, sans-serif; max-width: 40rem; margin: 3rem auto; padding: 0 1rem; color: #1c1c1c; }
	h1 { font-size: 1.4rem; } h1 code { font-size: 1em; }
	details { border: 1px solid #ddd; border-radius: 8px; margin: .5rem 0; }
	summary { cursor: pointer; padding: .6rem .9rem; font-weight: 600; }
	summary .count { color: #777; font-weight: 400; }
	ul { list-style: none; margin: 0; padding: .3rem .9rem .6rem; }
	li { padding: .25rem 0; display: flex; gap: .75rem; align-items: baseline; }
	a { font-weight: 500; }
	.upstream { color: #888; font-family: ui-monospace, monospace; font-size: .85em; }
	.empty { color: #777; }
`

// renderDashboard renders the full dashboard page. It is server-rendered
// (works without JS); the inline script re-fetches klank-agents.json
// every few seconds to pick up new agents without a reload, preserving
// which agents are expanded.
func renderDashboard(routes []route, domain string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">`)
	b.WriteString(`<title>klank services</title><style>`)
	b.WriteString(dashboardStyle)
	b.WriteString(`</style></head><body>`)
	fmt.Fprintf(&b, `<h1>klank services <code>&lt;port&gt;.&lt;agent&gt;.%s</code></h1><ul id="agents">`, html.EscapeString(domain))
	if len(routes) == 0 {
		b.WriteString(`<li class="empty">no agents with published ports</li>`)
	}
	groups := groupByAgent(routes)
	for i, g := range groups {
		fmt.Fprintf(&b, `<details id="agent-%d"%s><summary>%s <span class="count">(%d)</span></summary><ul>`,
			i, openAttr(i), html.EscapeString(g.agent), len(g.ports))
		for _, r := range g.ports {
			fmt.Fprintf(&b, `<li><a href="http://%s/">%s</a><span class="upstream">%s</span></li>`,
				html.EscapeString(r.fqdn), html.EscapeString(r.portName), html.EscapeString(r.upstream))
		}
		b.WriteString(`</ul></details>`)
	}
	b.WriteString(`</ul>`)
	b.WriteString(`<script>
const esc = s => { const d = document.createElement('div'); d.textContent = s; return d.innerHTML; };
function render(agents) {
	const open = new Set([...document.querySelectorAll('details[id^=agent-]')].filter(d => d.open).map(d => d.querySelector('summary').dataset.agent));
	const ul = document.getElementById('agents');
	ul.innerHTML = '';
	if (!agents.length) { ul.innerHTML = '<li class="empty">no agents with published ports</li>'; return; }
	agents.forEach(a => {
		const det = document.createElement('details');
		det.id = 'agent-' + agents.indexOf(a);
		if (open.has(a.name)) det.open = true;
		const sum = document.createElement('summary');
		sum.dataset.agent = a.name;
		sum.innerHTML = esc(a.name) + ' <span class="count">(' + a.ports.length + ')</span>';
		det.appendChild(sum);
		const list = document.createElement('ul');
		a.ports.forEach(p => {
			const li = document.createElement('li');
			const link = document.createElement('a');
			link.href = p.url; link.textContent = p.name;
			const up = document.createElement('span');
			up.className = 'upstream'; up.textContent = p.upstream;
			li.append(link, up); list.appendChild(li);
		});
		det.appendChild(list); ul.appendChild(det);
	});
}
async function refresh() {
	try { const r = await fetch('klank-agents.json'); render(await r.json()); } catch (e) {}
}
refresh(); setInterval(refresh, 5000);
</script>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

// openAttr expands the first agent so the list shows something useful
// even before JavaScript runs.
func openAttr(i int) string {
	if i == 0 {
		return " open"
	}
	return ""
}

// writeIfChanged compares before writing so unchanged polls are a no-op.
func writeIfChanged(path, content string) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == content {
		return false, nil
	}
	if err := writeFileAtomic(path, content); err != nil {
		return false, err
	}
	return true, nil
}

// writeFileAtomic writes via a temp file in the same directory and
// renames, so a concurrent caddy reload never sees a half-written file.
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return "/"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
