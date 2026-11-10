// Package command implements the klank subcommands (one file per command)
// plus their shared docker and tmux helpers.
package command

import (
	"bytes"
	"encoding/json"
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

// dockerImageCommand returns the image's configured entrypoint and cmd
// so the secrets wrapper can exec them after exporting the mounted
// secrets (empty when lookup/parse fails or the image sets neither).
func dockerImageCommand(image string) (entrypoint, cmd []string) {
	out, err := exec.Command("docker", "image", "inspect", image).Output()
	if err != nil {
		cli.Debug("image command lookup failed: %v", err)
		return nil, nil
	}
	var imgs []struct {
		Config struct {
			Entrypoint []string
			Cmd        []string
		}
	}
	if err := json.Unmarshal(out, &imgs); err != nil || len(imgs) == 0 {
		cli.Debug("image command parse failed: %v", err)
		return nil, nil
	}
	return imgs[0].Config.Entrypoint, imgs[0].Config.Cmd
}

// shQuote single-quotes a string for safe interpolation into a POSIX
// shell command (handles embedded quotes).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// secretsWrapper builds the body of a `sh -c` script that exports each
// file in /secrets as an env var, deletes the file immediately after
// reading it (bind mount: this removes it on the host too), then execs
// the image's original entrypoint/cmd, falling back to an interactive
// shell when the image sets neither.
func secretsWrapper(entrypoint, cmd []string) string {
	var b strings.Builder
	b.WriteString("set -eu\n" +
		"for f in /secrets/*; do\n" +
		"  [ -f \"$f\" ] || continue\n" +
		"  key=$(basename \"$f\")\n" +
		"  val=$(cat \"$f\")\n" +
		"  export \"$key=$val\"\n" +
		"  rm -f -- \"$f\"\n" +
		"done\n" +
		"exec")
	for _, a := range append(append([]string{}, entrypoint...), cmd...) {
		b.WriteString(" " + shQuote(a))
	}
	if len(entrypoint) == 0 && len(cmd) == 0 {
		b.WriteString(" sh")
	}
	b.WriteString("\n")
	return b.String()
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
