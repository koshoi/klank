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
