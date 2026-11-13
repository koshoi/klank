package main

import (
	"strings"
	"testing"
)

func TestRenderCaddyfileDefaultPort(t *testing.T) {
	routes := []route{
		{fqdn: "web.acme.klank.localhost", upstream: "http://127.0.0.1:1024"},
	}
	got := renderCaddyfile(routes, "klank.localhost", "/home/u/.klank", 80)
	for _, want := range []string{
		"http://klank.localhost {",
		"http://web.acme.klank.localhost {",
		"\treverse_proxy http://127.0.0.1:1024",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderCaddyfile() missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, ":80 {") {
		t.Errorf("renderCaddyfile() should not spell out the default port:\n%s", got)
	}
}

// TestRenderCaddyfileCustomPort: with -http-port 8080 the site addresses
// carry the listen port, so the supervised caddy binds 8080 while the
// Host header (portless, as sent by a root-run proxy on 80) still
// matches by hostname.
func TestAgentsJSONDashboardURLs(t *testing.T) {
	routes := []route{
		{fqdn: "web.acme.klank.localhost", upstream: "http://127.0.0.1:1024", portName: "http", agent: "acme"},
	}
	got := agentsJSON(routes, 8080)
	if !strings.Contains(got, "http://web.acme.klank.localhost:8080") {
		t.Errorf("agentsJSON(port 8080) missing ported URL:\n%s", got)
	}
	if strings.Contains(got, ":80\"") {
		t.Errorf("agentsJSON() should not spell out the default port:\n%s", got)
	}
	def := agentsJSON(routes, 80)
	if !strings.Contains(def, "http://web.acme.klank.localhost\"") {
		t.Errorf("agentsJSON(port 80) should emit portless URLs:\n%s", def)
	}
}

func TestRenderDashboardLinks(t *testing.T) {
	routes := []route{
		{fqdn: "web.acme.klank.localhost", upstream: "http://127.0.0.1:1024", portName: "http", agent: "acme"},
	}
	got := renderDashboard(routes, "klank.localhost", 8080)
	if !strings.Contains(got, `href="http://web.acme.klank.localhost:8080/"`) {
		t.Errorf("renderDashboard(port 8080) missing ported href:\n%s", got)
	}
	def := renderDashboard(routes, "klank.localhost", 80)
	if !strings.Contains(def, `href="http://web.acme.klank.localhost/"`) {
		t.Errorf("renderDashboard(port 80) should emit portless hrefs:\n%s", def)
	}
}

func TestRenderCaddyfileCustomPort(t *testing.T) {
	routes := []route{
		{fqdn: "web.acme.klank.localhost", upstream: "http://127.0.0.1:1024"},
	}
	got := renderCaddyfile(routes, "klank.localhost", "/home/u/.klank", 8080)
	for _, want := range []string{
		"http://klank.localhost:8080 {",
		"http://web.acme.klank.localhost:8080 {",
		"\treverse_proxy http://127.0.0.1:1024",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderCaddyfile() missing %q in:\n%s", want, got)
		}
	}
}
