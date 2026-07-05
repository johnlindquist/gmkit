package cmd_test

import (
	"strings"
	"testing"
)

// TestVisualSmoke prints the human-formatted output of each query command
// into the test log. Run with `go test -v -run TestVisualSmoke ./cmd/` to
// inspect rendering manually. Skipped under `go test` without -v.
func TestVisualSmoke(t *testing.T) {
	if !testing.Verbose() {
		t.Skip("set -v to see rendered output")
	}
	dir := seedStore(t)
	scenarios := [][]string{
		{"chats", "list"},
		{"messages", "search", "dinner"},
		{"messages", "context", "m3", "--before", "1", "--after", "1"},
		{"messages", "show", "m3"},
		{"contacts", "search", "alice"},
		{"contacts", "show", "+15555550100"},
		{"chats", "show", "c_alice"},
	}
	for _, args := range scenarios {
		out := runCmd(t, dir, args...)
		t.Logf("\n$ gmcli %s\n---\n%s\n---", strings.Join(args, " "), out)
	}
}
