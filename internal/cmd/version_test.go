package cmd

import "testing"

func TestDefaultVersionIsNotHardcodedRelease(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("default Version = %q, want dev; release tags must be injected at build time", Version)
	}
}

func TestBuildInfoUsesExplicitVersionOverride(t *testing.T) {
	old := Version
	Version = "v9.9.9-test"
	t.Cleanup(func() { Version = old })

	info := buildInfo()
	if info.Version != "v9.9.9-test" {
		t.Fatalf("version = %q, want explicit override", info.Version)
	}
}
