// Package config loads klank's TOML configuration.
//
// Config is loaded with the standard software config priority hierarchy
// (lowest to highest; later sources override earlier ones):
//
//  1. built-in defaults
//  2. system config:  /etc/klank/config.toml
//  3. user config:    $XDG_CONFIG_HOME/klank/config.toml
//     (default ~/.config/klank/config.toml)
//  4. project config: ./.klank.toml (current working directory)
//  5. environment:    KLANK_IMAGE
//  6. CLI arguments
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"klank/cli"
)

// Config holds the configurable klank settings.
type Config struct {
	Image   string         `toml:"image"`    // default container image for "klank new"
	EnvFile string         `toml:"env_file"` // path to a default env file passed to the container
	IPv6    *bool          `toml:"ipv6"`     // launch agents on an IPv6-enabled docker network (default false; nil means default)
	Ports   map[string]int `toml:"ports"`    // default named ports published for every new agent (see [ports] in config.toml)
}

// DefaultIPv6 is the built-in default for IPv6 agent networking. Agents run
// on the default (IPv4-only) bridge unless IPv6 is explicitly enabled via
// config (ipv6 = true) or the --ipv6 flag. Outbound IPv6 additionally
// requires the host to have IPv6 connectivity; when the network cannot be
// set up, klank warns and falls back to the default bridge.
const DefaultIPv6 = false

// UseIPv6 reports whether agents should use the IPv6 docker network.
func (c *Config) UseIPv6() bool {
	if c.IPv6 == nil {
		return DefaultIPv6
	}
	return *c.IPv6
}

// DefaultImage is the built-in default container image.
const DefaultImage = "klank:latest"

// SystemConfigPath is the system-level config file path.
const SystemConfigPath = "/etc/klank/config.toml"

// UserConfigPath returns the user-level config file path.
func UserConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "klank", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		cli.Debug("cannot determine home dir: %v", err)
		return ""
	}
	return filepath.Join(home, ".config", "klank", "config.toml")
}

// loadConfigFile reads a single TOML file into cfg, ignoring missing files.
func loadConfigFile(path, desc string, cfg *Config) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			cli.Debug("read %s config %s: %v", desc, path, err)
		}
		return
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		cli.Fatal("invalid %s config %s: %v", desc, path, err)
	}
	cli.Debug("loaded %s config: %s", desc, path)
}

// Load assembles the effective config according to the hierarchy above.
func Load() *Config {
	cfg := &Config{Image: DefaultImage}
	for _, path := range []string{
		SystemConfigPath, // system
		UserConfigPath(), // user
		".klank.toml",    // project
	} {
		if path == "" {
			continue
		}
		loadConfigFile(path, "config", cfg)
	}
	// environment overrides file configs
	if v := os.Getenv("KLANK_IMAGE"); v != "" {
		cfg.Image = v
	}
	for name, port := range cfg.Ports {
		if port < 1 || port > 65535 {
			cli.Fatal("invalid config [ports] entry %s = %d (expected 1-65535)", name, port)
		}
	}
	return cfg
}

// ParseEnvFile reads a dotenv-style file (KEY=VALUE per line, supports
// "export " prefix, # comments, blank lines) into a map.
func ParseEnvFile(path string) map[string]string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cli.MustCwd(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cli.Fatal("env file not found: %s", path)
		}
		cli.Fatal("read env file %s: %v", path, err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			cli.Fatal("invalid line in env file %s: %q", path, line)
		}
		env[strings.TrimSpace(k)] = unquoteEnvValue(strings.TrimSpace(v))
	}
	return env
}

// unquoteEnvValue strips surrounding single/double quotes if present.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 && (v[0] == v[len(v)-1]) && (v[0] == '"' || v[0] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}
