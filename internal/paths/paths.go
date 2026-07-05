// Package paths resolves the on-disk locations gmcli reads and writes.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout describes the canonical file layout under a single store directory.
type Layout struct {
	Root     string // store root
	Session  string // session.json (libgm AuthData)
	Database string // gmcli.db (SQLite + FTS5)
	MediaDir string // media/ (downloaded attachment cache)
	Socket   string // gmcli.sock (`gmcli serve` unix socket)
}

// Resolve returns the layout rooted at storeOverride if non-empty, otherwise
// at $XDG_STATE_HOME/gmcli (falling back to $HOME/.local/state/gmcli). The
// store directory holds session state, the SQLite archive, and cached media —
// all of which are reproducible from the phone, so XDG_STATE_HOME is the
// correct base under the spec. Matches wacli's choice.
func Resolve(storeOverride string) (Layout, error) {
	root := storeOverride
	if root == "" {
		var err error
		root, err = defaultRoot()
		if err != nil {
			return Layout{}, err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve store path: %w", err)
	}
	return Layout{
		Root:     abs,
		Session:  filepath.Join(abs, "session.json"),
		Database: filepath.Join(abs, "gmcli.db"),
		MediaDir: filepath.Join(abs, "media"),
		Socket:   filepath.Join(abs, "gmcli.sock"),
	}, nil
}

// DaemonLog is where auto-started daemons write their output.
func (l Layout) DaemonLog() string {
	return filepath.Join(l.Root, "daemon.log")
}

// EnsureDirs creates the store root and media subdirectory with 0700.
func (l Layout) EnsureDirs() error {
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return fmt.Errorf("create store dir %s: %w", l.Root, err)
	}
	if err := os.MkdirAll(l.MediaDir, 0o700); err != nil {
		return fmt.Errorf("create media dir %s: %w", l.MediaDir, err)
	}
	return nil
}

func defaultRoot() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "gmcli"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "gmcli"), nil
}
