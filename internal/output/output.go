// Package output provides shared rendering helpers for cobra commands.
// Subcommands hand a typed value to JSON or to a renderer; --json switches
// the entire program between the two without per-command branching.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// JSON encodes v to w as indented JSON followed by a newline.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Table writes a tab-aligned table. headers are uppercased and printed once;
// rows is a slice of equal-length string slices.
func Table(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(upper(headers), "\t")); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(r, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// FormatTime renders a millisecond Unix timestamp in local time, or "—" for
// the epoch (used as a sentinel for "not set").
func FormatTime(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

// Truncate caps a string to a rune length, replacing the tail with an
// ellipsis. Useful for table cells with chat bodies.
func Truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ↵ ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}
