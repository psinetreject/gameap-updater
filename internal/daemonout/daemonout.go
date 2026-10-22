// Package daemonout removes the framing gameap-daemon wraps around a command's
// output, so callers see only what the command itself printed.
package daemonout

import (
	"regexp"
	"strings"
)

// The daemon writes "<work dir># <command>\n\n" before the output and
// "\nExited with <code>\n" after it (gameap/daemon
// internal/app/components/executor.go). The exit code also arrives on its own,
// so nothing is lost by dropping the footer.
var (
	header = regexp.MustCompile(`\A[^\n]*# [^\n]*\n\n`)
	footer = regexp.MustCompile(`\n?Exited with -?\d+\n?\z`)
)

// Strip returns output without the daemon's header and footer. Output without
// them is returned unchanged.
func Strip(output string) string {
	output = header.ReplaceAllString(output, "")
	output = footer.ReplaceAllString(output, "")

	return strings.TrimRight(output, "\n")
}
