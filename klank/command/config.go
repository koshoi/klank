package command

import (
	"fmt"
	"os"

	"klank/cli"
	"klank/config"
)

// projectConfigFile is the per-project config `klank config` writes.
const projectConfigFile = ".klank.toml"

// configTemplate is written by `klank config` when neither a user nor a
// system config exists: a commented starter showing every supported key.
const configTemplate = `# klank project config (.klank.toml)
# Overrides the user config (~/.config/klank/config.toml) for this directory.

# default container image for "klank new"
# image = "klank:latest"

# dotenv-style file whose KEY=VALUE lines are passed to new containers
# env_file = ".env"

# launch agents on an IPv6-enabled docker network
# ipv6 = false

[ports]
# named ports published on 127.0.0.1 for every new agent in this project;
# the local port is picked free, close to the container port. Override or
# extend per launch with -p name=container-port. Mappings land in the
# agent database (` + "`" + `klank port` + "`" + `) and in the container's /ports file.
# http = 80
# grpc = 8000
`

// fileExists reports whether path exists (any file type).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Config implements `klank config`: copy the current config into
// ./.klank.toml for easier per-project editing. The source is the user
// config (~/.config/klank/config.toml), falling back to the system config
// (/etc/klank/config.toml); when neither exists, a commented starter
// template with all supported keys is written instead. An existing
// ./.klank.toml is only overwritten with --force.
func Config(args []string) {
	force := false
	for _, a := range args {
		switch a {
		case "-f", "--force":
			force = true
		default:
			cli.Fatal("unexpected argument: %s (usage: klank config [-f])", a)
		}
	}
	if fileExists(projectConfigFile) {
		if !force {
			cli.Fatal("%s already exists (edit it in place, or pass -f to overwrite it)", projectConfigFile)
		}
		cli.Debug("%s exists; overwriting (--force)", projectConfigFile)
	}

	src := config.UserConfigPath()
	switch {
	case src != "" && fileExists(src):
		// user config wins
	case fileExists(config.SystemConfigPath):
		src = config.SystemConfigPath
	default:
		src = ""
	}

	if src == "" {
		if err := os.WriteFile(projectConfigFile, []byte(configTemplate), 0o644); err != nil {
			cli.Fatal("write %s: %v", projectConfigFile, err)
		}
		fmt.Printf("wrote %s (starter template; no user or system config found)\n", projectConfigFile)
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		cli.Fatal("read %s: %v", src, err)
	}
	if err := os.WriteFile(projectConfigFile, data, 0o644); err != nil {
		cli.Fatal("write %s: %v", projectConfigFile, err)
	}
	fmt.Printf("copied %s -> %s\n", src, projectConfigFile)
}
