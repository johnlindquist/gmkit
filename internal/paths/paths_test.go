package paths_test

import (
	"path/filepath"
	"testing"

	"github.com/fdsouvenir/gmcli/internal/paths"
)

func TestResolveExplicit(t *testing.T) {
	dir := t.TempDir()
	got, err := paths.Resolve(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Root != dir {
		t.Errorf("root = %s, want %s", got.Root, dir)
	}
	if got.Database != filepath.Join(dir, "gmcli.db") {
		t.Errorf("db = %s", got.Database)
	}
	if got.Session != filepath.Join(dir, "session.json") {
		t.Errorf("session = %s", got.Session)
	}
	if got.MediaDir != filepath.Join(dir, "media") {
		t.Errorf("media = %s", got.MediaDir)
	}
}

func TestEnsureDirsCreatesTree(t *testing.T) {
	dir := t.TempDir()
	layout, err := paths.Resolve(filepath.Join(dir, "nested", "gmcli"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := layout.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	if _, err := paths.Resolve(layout.Root); err != nil {
		t.Errorf("re-resolve: %v", err)
	}
}

func TestResolveRespectsXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")
	got, err := paths.Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Root != "/tmp/xdg-test/gmcli" {
		t.Errorf("root = %s, want /tmp/xdg-test/gmcli", got.Root)
	}
}
