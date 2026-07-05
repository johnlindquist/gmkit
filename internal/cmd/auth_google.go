package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/johnlindquist/gmkit/internal/gm"
)

func authGoogleCmd() *cobra.Command {
	var cookiesFile string
	c := &cobra.Command{
		Use:   "google",
		Short: "Pair with Google Messages using Google account emoji confirmation",
		Long: "Pair with Google Messages using the Google account flow instead of QR. " +
			"Cookie input is read from a file or stdin and is never printed; the resulting " +
			"session is saved to $STORE/session.json (mode 0600).",
		RunE: func(cmd *cobra.Command, args []string) error {
			layout, err := resolveLayout()
			if err != nil {
				return err
			}
			cookies, err := readGoogleCookieInput(cookiesFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			logger := newLogger()
			ctx, cancel := signalContext(cmd.Context())
			defer cancel()

			// A running daemon holds the old session; stop it so the next
			// client auto-starts a fresh daemon with the new pairing.
			stopRunningDaemon(ctx, layout.Socket)

			fmt.Fprintln(os.Stderr, "Starting Google account pairing...")
			res, err := gm.PairGoogle(ctx, layout, logger, cookies, func(emoji string) {
				fmt.Fprintf(os.Stderr, "On your phone, tap this emoji in Google Messages: %s\n", emoji)
				fmt.Fprintln(os.Stderr, "Waiting for confirmation... (Ctrl-C to cancel)")
			})
			if err != nil {
				return fmt.Errorf("pair google: %w", err)
			}
			fmt.Fprintf(os.Stderr, "Paired. phone_id=%s session=%s\n", res.PhoneID, res.SessionPath)
			return nil
		},
	}
	c.Flags().StringVar(&cookiesFile, "cookies-file", "-", "path to cookie JSON/cURL input, or - for stdin")
	return c
}

func readGoogleCookieInput(path string, stdin io.Reader) (map[string]string, error) {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read cookie input: %w", err)
	}
	return gm.ParseGoogleCookieInput(string(data))
}

// parseGoogleCookieInput is a thin alias kept for the package tests.
func parseGoogleCookieInput(input string) (map[string]string, error) {
	return gm.ParseGoogleCookieInput(input)
}

// requiredGoogleCookies mirrors the gm package's list for the tests.
var requiredGoogleCookies = []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}
