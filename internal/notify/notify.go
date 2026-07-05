// Package notify sends best-effort desktop notifications for incoming
// messages. macOS uses osascript; Linux uses notify-send when present.
// Failures are returned for logging but should never be fatal — a missing
// notifier must not take the daemon down.
package notify

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Send shows one desktop notification.
func Send(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s",
			appleScriptString(body), appleScriptString(title))
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			return fmt.Errorf("notify-send not installed")
		}
		return exec.Command("notify-send", "--app-name=gmkit", title, body).Run()
	default:
		return fmt.Errorf("desktop notifications unsupported on %s", runtime.GOOS)
	}
}

// appleScriptString quotes a Go string as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
