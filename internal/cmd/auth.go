package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"

	"github.com/johnlindquist/gmkit/internal/gm"
	"github.com/johnlindquist/gmkit/internal/rpc"
)

func authCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "auth",
		Short: "Pair with Google Messages by scanning a QR code",
		Long: "Run once to pair gmcli with your phone. The command renders a QR code; " +
			"open Google Messages on your phone, go to Settings -> Device pairing -> QR code " +
			"scanner, and scan the code. The session is saved to $STORE/session.json (mode 0600).",
		RunE: func(cmd *cobra.Command, args []string) error {
			layout, err := resolveLayout()
			if err != nil {
				return err
			}
			logger := newLogger()
			ctx, cancel := signalContext(context.Background())
			defer cancel()

			// A running daemon holds the old (dead) session; stop it so the
			// next client auto-starts a fresh daemon with the new pairing.
			stopRunningDaemon(ctx, layout.Socket)

			fmt.Fprintln(os.Stderr, "Requesting pairing token from Google...")
			res, err := gm.Pair(ctx, layout, logger, func(qrURL string) {
				fmt.Fprintln(os.Stderr, "Scan this QR code from Google Messages -> Device pairing:")
				fmt.Fprintln(os.Stderr)
				qrterminal.GenerateHalfBlock(qrURL, qrterminal.L, os.Stderr)
				fmt.Fprintln(os.Stderr)
				fmt.Fprintln(os.Stderr, "Or paste this URL into a QR generator:")
				fmt.Fprintln(os.Stderr, "  ", qrURL)
				fmt.Fprintln(os.Stderr)
				fmt.Fprintln(os.Stderr, "Waiting for pairing... (Ctrl-C to cancel)")
			})
			if err != nil {
				return fmt.Errorf("pair: %w", err)
			}
			fmt.Fprintf(os.Stderr, "Paired. phone_id=%s session=%s\n", res.PhoneID, res.SessionPath)
			fmt.Fprintln(os.Stderr, "All set — launch gmtui (or any gmcli command); the daemon starts itself with the new session.")
			return nil
		},
	}
	c.AddCommand(authGoogleCmd())
	return c
}

// stopRunningDaemon asks a live daemon on the socket to exit and waits
// briefly for it to go. Best-effort: no daemon, no problem.
func stopRunningDaemon(ctx context.Context, socket string) {
	client, err := rpc.Dial(socket)
	if err != nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = client.Call(callCtx, "daemon.shutdown", nil, nil)
	_ = client.Close()
	fmt.Fprintln(os.Stderr, "Stopped the running gmkit daemon (it restarts on next use with the new session).")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := rpc.Dial(socket); err != nil {
			return
		} else {
			_ = c.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
}
