package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/gm"
)

var requiredGoogleCookies = []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}
var optionalGoogleCookies = []string{"__Secure-1PSIDTS"}
var fetchCookieHeaderPattern = regexp.MustCompile(`(?is)(?:^|[,{]\s*)["']?cookie["']?\s*:\s*["']([^"']+)["']`)

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
	return parseGoogleCookieInput(string(data))
}

func parseGoogleCookieInput(input string) (map[string]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("cookie input is empty")
	}
	var cookies map[string]string
	if strings.HasPrefix(input, "{") {
		if err := json.Unmarshal([]byte(input), &cookies); err != nil {
			return nil, fmt.Errorf("parse cookie JSON: %w", err)
		}
	} else {
		header := extractCookieHeader(input)
		cookies = parseCookieHeader(header)
	}
	cookies = filterGoogleCookies(cookies)
	if err := validateGoogleCookies(cookies); err != nil {
		return nil, err
	}
	return cookies, nil
}

func extractCookieHeader(input string) string {
	if header, ok := extractFetchCookieHeader(input); ok {
		return header
	}
	if header, ok := extractAfterCookieLabel(input); ok {
		return header
	}
	if header, ok := extractAfterCookieFlag(input, "--cookie"); ok {
		return header
	}
	if header, ok := extractAfterCookieFlag(input, "-b"); ok {
		return header
	}
	return input
}

func extractFetchCookieHeader(input string) (string, bool) {
	matches := fetchCookieHeaderPattern.FindStringSubmatch(input)
	if len(matches) < 2 {
		return "", false
	}
	return strings.TrimSpace(matches[1]), true
}

func extractAfterCookieLabel(input string) (string, bool) {
	idx := strings.Index(strings.ToLower(input), "cookie:")
	if idx < 0 {
		return "", false
	}
	quote := byte(0)
	if idx > 0 && (input[idx-1] == '\'' || input[idx-1] == '"') {
		quote = input[idx-1]
	}
	rest := input[idx+len("cookie:"):]
	if quote != 0 {
		if end := strings.IndexByte(rest, quote); end >= 0 {
			rest = rest[:end]
		}
	} else if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest), true
}

func extractAfterCookieFlag(input, flag string) (string, bool) {
	idx := strings.Index(input, flag)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(input[idx+len(flag):])
	if rest == "" {
		return "", false
	}
	if rest[0] == '\'' || rest[0] == '"' {
		quote := rest[0]
		rest = rest[1:]
		if end := strings.IndexByte(rest, quote); end >= 0 {
			rest = rest[:end]
		}
		return strings.TrimSpace(rest), true
	}
	for i, r := range rest {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return strings.TrimSpace(rest[:i]), true
		}
	}
	return strings.TrimSpace(rest), true
}

func parseCookieHeader(header string) map[string]string {
	cookies := make(map[string]string)
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "cookie:") {
		header = strings.TrimSpace(header[len("cookie:"):])
	}
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name != "" && value != "" {
			cookies[name] = value
		}
	}
	return cookies
}

func filterGoogleCookies(cookies map[string]string) map[string]string {
	filtered := make(map[string]string)
	for _, name := range requiredGoogleCookies {
		if value := strings.TrimSpace(cookies[name]); value != "" {
			filtered[name] = value
		}
	}
	for _, name := range optionalGoogleCookies {
		if value := strings.TrimSpace(cookies[name]); value != "" {
			filtered[name] = value
		}
	}
	return filtered
}

func validateGoogleCookies(cookies map[string]string) error {
	var missing []string
	for _, name := range requiredGoogleCookies {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		if len(missing) == len(requiredGoogleCookies) {
			return fmt.Errorf("cookie input did not contain the required Google cookies; copy the /web/config request as cURL (bash), Copy as fetch, or copy the Cookie request header")
		}
		return fmt.Errorf("cookie input missing required Google cookies: %s", strings.Join(missing, ", "))
	}
	return nil
}
