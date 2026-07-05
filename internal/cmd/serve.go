package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlindquist/gmkit/internal/gm"
	"github.com/johnlindquist/gmkit/internal/rpc"
	"github.com/johnlindquist/gmkit/internal/store"
	gmsync "github.com/johnlindquist/gmkit/internal/sync"
)

func serveCmd() *cobra.Command {
	var sends string
	var socketPath string
	var noImport bool
	var offline bool
	var auto bool
	var idleExit time.Duration
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the gmcli daemon: keep syncing and expose a local RPC socket",
		Long: "Keep the Google Messages session alive, stream events into the local " +
			"archive, and expose a JSON-RPC surface over a unix socket for clients " +
			"like gmtui and agent integrations (see `gmcli mcp`).\n\n" +
			"Sends are controlled by two layers: the daemon is read-only unless " +
			"started with --read-only=false, and even then sends default to an " +
			"approval queue (--sends approve) where a human confirms each outgoing " +
			"message. --sends direct skips the queue but still records an audit row.\n\n" +
			"--offline serves the local archive without connecting to the phone: " +
			"queries and the approval queue work, but nothing can be sent or " +
			"backfilled until a daemon with a connection is running.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if auto {
				// Auto-started daemons default to the approval queue (safe:
				// nothing reaches the phone without a human approving) and
				// retire themselves when unused. Explicit flags still win.
				if !cmd.Root().PersistentFlags().Changed("read-only") {
					flags.readOnly = false
				}
				if !cmd.Flags().Changed("idle-exit") {
					idleExit = 10 * time.Minute
				}
			}
			mode := rpc.SendOff
			if !flags.readOnly {
				switch sends {
				case "approve":
					mode = rpc.SendApprove
				case "direct":
					mode = rpc.SendDirect
				default:
					return fmt.Errorf("--sends must be approve or direct, got %q", sends)
				}
			}

			layout, err := resolveLayout()
			if err != nil {
				return err
			}
			logger := newLogger()

			ctx, cancel := signalContext(context.Background())
			defer cancel()

			st, err := store.Open(ctx, layout.Database)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer st.Close()

			var client *gm.Client
			var pump *gmsync.Pump
			if !offline {
				client, err = gm.Open(layout, logger)
				if err != nil {
					return err
				}
				pump = gmsync.New(st, logger)
			}
			server := rpc.NewServer(rpc.Deps{
				Store:    st,
				Client:   client,
				Pump:     pump,
				Logger:   logger,
				Version:  Version,
				SendMode: mode,
				IdleExit: idleExit,
			})
			if client != nil {
				// Order matters: the pump persists the row first, then the
				// broadcaster tells subscribers about it.
				client.Subscribe(pump.Handle)
				client.Subscribe(server.HandleGMEvent)
			}

			sock := socketPath
			if sock == "" {
				sock = layout.Socket
			}
			ln, err := rpc.Listen(sock)
			if err != nil {
				return err
			}

			// Accept clients immediately: archive queries work while the
			// phone connection and initial import proceed in the
			// background, so auto-started daemons feel instant.
			serveErr := make(chan error, 1)
			go func() { serveErr <- server.Serve(ctx, ln) }()
			fmt.Fprintf(os.Stderr, "Daemon ready. RPC socket: %s (send mode: %s). Ctrl-C to stop.\n", sock, mode)

			if client != nil {
				fmt.Fprintln(os.Stderr, "Connecting to Google Messages relay...")
				if err := client.Connect(); err != nil {
					return fmt.Errorf("connect: %w", err)
				}
				defer client.Disconnect()

				if !noImport {
					runInitialImport(ctx, client, pump, logger)
				}
			}

			var pumpFatal <-chan error // nil (blocks forever) in offline mode
			if pump != nil {
				pumpFatal = pump.Fatal()
			}
			heartbeat := time.NewTicker(syncHeartbeatInterval)
			defer heartbeat.Stop()
			for {
				select {
				case <-ctx.Done():
					fmt.Fprintln(os.Stderr, "Shutting down...")
					return nil
				case err := <-pumpFatal:
					return err
				case err := <-serveErr:
					if err != nil {
						return fmt.Errorf("rpc server: %w", err)
					}
					return nil
				case <-server.Idle():
					fmt.Fprintf(os.Stderr, "No clients for %s; exiting (auto mode).\n", idleExit)
					return nil
				case <-heartbeat.C:
					if err := st.TouchSync(ctx); err != nil {
						logger.Debug().Err(err).Msg("sync heartbeat failed")
					}
				}
			}
		},
	}
	c.Flags().StringVar(&sends, "sends", "approve", "send policy when --read-only=false: approve (queue for human approval) or direct")
	c.Flags().StringVar(&socketPath, "socket", "", "unix socket path (default: <store>/gmcli.sock)")
	c.Flags().BoolVar(&noImport, "no-import", false, "skip the initial contact/conversation import on startup")
	c.Flags().BoolVar(&offline, "offline", false, "serve the local archive without connecting to the phone")
	c.Flags().BoolVar(&auto, "auto", false, "on-demand mode used by gmtui/mcp: approval-gated sends, exits when idle")
	c.Flags().DurationVar(&idleExit, "idle-exit", 0, "exit after this long with no connected clients (0 = never)")
	return c
}
