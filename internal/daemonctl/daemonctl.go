// Package daemonctl starts the gmcli daemon on demand. Clients (the
// approvals CLI, the MCP server, gmtui) call EnsureRunning before dialing
// the socket; if nothing is listening, it spawns `gmcli serve --auto` as a
// detached process and waits for the socket to come up. Auto-started
// daemons retire themselves after an idle period, so users never manage
// daemon lifecycle by hand.
package daemonctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/johnlindquist/gmkit/internal/paths"
	"github.com/johnlindquist/gmkit/internal/rpc"
)

// Options tunes EnsureRunning.
type Options struct {
	// LogLevel is passed through to the spawned daemon (default "info").
	LogLevel string
	// Offline forces `serve --auto --offline` (no phone connection).
	Offline bool
	// StartTimeout bounds the wait for the socket (default 30s).
	StartTimeout time.Duration
	// IdleExit overrides the spawned daemon's idle-exit duration.
	IdleExit time.Duration
}

// EnsureRunning makes sure a daemon is listening on layout.Socket, spawning
// one if needed. A store with no paired session falls back to an offline
// daemon (archive queries and the approval queue still work; sends and
// backfills report unavailable until the user pairs).
func EnsureRunning(ctx context.Context, layout paths.Layout, opts Options) error {
	if alive(layout.Socket) {
		return nil
	}
	if opts.LogLevel == "" {
		opts.LogLevel = "info"
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 30 * time.Second
	}
	offline := opts.Offline
	if !offline {
		if _, err := os.Stat(layout.Session); err != nil {
			// Not paired: an online daemon would just crash. Serve the
			// archive; auth guidance surfaces on send/backfill attempts.
			offline = true
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate gmcli binary: %w", err)
	}
	args := []string{
		"--store", layout.Root,
		"--log-level", opts.LogLevel,
		"serve", "--auto",
	}
	if offline {
		args = append(args, "--offline")
	}
	if opts.IdleExit > 0 {
		args = append(args, "--idle-exit", opts.IdleExit.String())
	}

	if err := layout.EnsureDirs(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(layout.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// New session: the daemon must survive this process and ignore its
	// terminal signals (Ctrl-C in gmtui must not kill the daemon).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start gmcli daemon: %w", err)
	}
	// Reap the child when it eventually exits so it never zombifies while
	// this (possibly long-lived) client is around.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(opts.StartTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if alive(layout.Socket) {
			return nil
		}
		// If the daemon died immediately (bad state, port conflict), stop
		// waiting and point at its log.
		if cmd.ProcessState != nil {
			return fmt.Errorf("gmcli daemon exited during startup; see %s", layout.DaemonLog())
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("gmcli daemon did not come up within %s; see %s", opts.StartTimeout, layout.DaemonLog())
}

// alive reports whether a daemon answers a ping on the socket.
func alive(socket string) bool {
	client, err := rpc.Dial(socket)
	if err != nil {
		return false
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res struct {
		Pong bool `json:"pong"`
	}
	return client.Call(ctx, "ping", nil, &res) == nil && res.Pong
}
