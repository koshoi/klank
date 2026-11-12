// Package klankdb defines the on-disk contract shared by klank and
// klankd: where the agents database lives and the JSON format of its
// ports column.
//
// It is deliberately a leaf package — no cli import, no os.Exit, no
// sql.Open — so the long-running klankd daemon (and any other binary
// that must survive errors instead of dying) can share it safely.
// Schema-breaking changes are caught at compile time by both binaries.
//
// The database itself is owned by klank's storage package: it creates
// and migrates the schema. klankd only reads it (mode=ro).
package klankdb

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
)

// PortMapping describes one named container port published to the host's
// loopback (see `klank new -p name=container-port`). Name is the handle
// used by `klank port <agent> <name>`; Container is the port inside the
// container, Host the port published on 127.0.0.1.
//
// This is also the JSON schema of the agents.ports column: klank stores
// it there, klankd decodes it to build its routes. Changing the json
// tags changes the database format.
type PortMapping struct {
	Name      string `json:"name"`
	Container int    `json:"container"`
	Host      int    `json:"host"`
}

// ErrNoHome reports that no home directory could be determined.
var ErrNoHome = errors.New("cannot determine home directory")

// Dir returns klank's data directory under the given home directory
// (empty string = the current user's home).
func Dir(home string) (string, error) {
	if home == "" {
		var err error
		if home, err = homeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, ".klank"), nil
}

// DefaultPath returns the default database path (~/.klank/klank.db)
// under the given home directory (empty string = the current user's
// home). Used as storage.Open's location and klankd's -db default.
func DefaultPath(home string) (string, error) {
	dir, err := Dir(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "klank.db"), nil
}

// homeDir resolves the current user's home directory: $HOME via
// os.UserHomeDir, falling back to the passwd entry (klankd can run
// early in boot, before $HOME is meaningful).
func homeDir() (string, error) {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h, nil
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	return "", ErrNoHome
}
