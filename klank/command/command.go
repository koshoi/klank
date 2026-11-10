// Package command implements the klank subcommands (one file per command)
// plus their shared docker and tmux helpers.
package command

import (
	"bytes"
	"os"
	"os/exec"
	"strings"

	"klank/cli"
)

// dockerState classifies the result of checking one container.
type dockerState int

const (
	dockerRunning dockerState = iota // container exists and is running
	dockerStopped                    // container exists but is not running
	dockerMissing                    // no such container
	dockerUnknown                    // docker itself failed (e.g. daemon down)
)

// currentUserIDs returns the invoking user's uid/gid so container files
// map 1:1 to their host ownership.
func currentUserIDs() (int, int) {
	uid, gid := os.Getuid(), os.Getgid()
	cli.Debug("running as uid=%d gid=%d", uid, gid)
	return uid, gid
}

// dockerImageHash returns the image digest of the container's image
// (e.g. sha256:...) via docker inspect.
func dockerImageHash(cid string) string {
	out, err := exec.Command("docker", "inspect", "--format", "{{.Image}}", cid).Output()
	if err != nil {
		cli.Debug("image hash lookup failed: %v", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerAttach attaches the current terminal to a running container.
func dockerAttach(cid string) {
	attach := exec.Command("docker", "attach", cid)
	attach.Stdin = os.Stdin
	attach.Stdout = os.Stdout
	attach.Stderr = os.Stderr
	if err := attach.Run(); err != nil {
		cli.Fatal("docker attach: %v", err)
	}
}

// dockerIsRunning reports whether the container is in the running state.
func dockerIsRunning(cid string) bool {
	return exec.Command("docker", "inspect", "-f", "{{.State.Running}}", cid).Run() == nil
}

// dockerAlive classifies a container's state via docker inspect. The
// dockerUnknown case matters for pruning: when docker itself is broken
// (daemon down, no permission), every lookup fails, so callers must not
// treat that as "container gone".
func dockerAlive(cid string) dockerState {
	cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", cid)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "No such object") {
			return dockerMissing
		}
		cli.Debug("docker inspect %s failed: %v (%s)", cid, err, strings.TrimSpace(stderr.String()))
		return dockerUnknown
	}
	if strings.TrimSpace(string(out)) != "true" {
		return dockerStopped
	}
	return dockerRunning
}
