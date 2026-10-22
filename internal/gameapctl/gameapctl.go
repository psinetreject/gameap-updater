// Package gameapctl builds the fixed commands the plugin runs on a node and
// reads what they print. Nothing here talks to the panel, so it is tested with
// plain `go test ./internal/...`.
package gameapctl

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var (
	// ErrUnsupportedOS means the node reports an OS this plugin has no command
	// template for.
	ErrUnsupportedOS = errors.New("unsupported node OS")
	// ErrNotFound means the launcher ran but found no gameapctl on the
	// daemon's PATH.
	ErrNotFound = errors.New("gameapctl not found on the daemon's PATH")
	// ErrDaemonNotFound means the launcher ran but found no gameap-daemon
	// binary on the daemon's PATH.
	ErrDaemonNotFound = errors.New("gameap-daemon not found on the daemon's PATH")
	// ErrPanelNotFound means the launcher found no GameAP panel binary on the
	// daemon's PATH: the node does not host a panel.
	ErrPanelNotFound = errors.New("no GameAP panel binary on the daemon's PATH")
	// ErrUnrecognisedOutput means the program ran but printed no version line.
	ErrUnrecognisedOutput = errors.New("unrecognised version output")
)

// The daemon runs a command without a shell: it splits the string and starts
// the program it names. Every command starts with an absolute, system-owned
// launcher that searches PATH only, never a bare program name.
const (
	// /usr/bin/env is at this path on every mainstream Linux distribution,
	// BusyBox-based ones included, and never looks in the current directory.
	unixLauncher = "/usr/bin/env"
	// PowerShell does not run programs from the current directory unless
	// they are written as .\name. The daemon doubles backslashes before it
	// splits the string on Windows, so the path needs no quoting.
	windowsLauncher = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe -NoProfile -NonInteractive -Command`
)

// VersionCommand returns the command that prints gameapctl's version on a node
// running nodeOS, as the panel reports it ("linux" or "windows").
func VersionCommand(nodeOS string) (string, error) {
	return versionCommand(nodeOS, "gameapctl version")
}

// DaemonVersionCommand returns the command that prints the version of the
// gameap-daemon binary on a node. It reads the binary on disk, which after an
// upgrade is the one the restarted daemon runs.
func DaemonVersionCommand(nodeOS string) (string, error) {
	return versionCommand(nodeOS, "gameap-daemon version")
}

// PanelVersionCommand returns the command that prints the version of the GameAP
// panel binary on a node, if the node hosts one. --version is what gameapctl
// itself asks; the panel answers it from v4.5 on and prints its usage before.
func PanelVersionCommand(nodeOS string) (string, error) {
	return versionCommand(nodeOS, "gameap --version")
}

func versionCommand(nodeOS, programAndArgs string) (string, error) {
	switch strings.ToLower(nodeOS) {
	case "linux":
		return unixLauncher + " " + programAndArgs, nil
	case "windows":
		return windowsLauncher + ` "` + programAndArgs + `"`, nil
	default:
		return "", ErrUnsupportedOS
	}
}

var (
	// `gameapctl version` prints "Version: v0.34.0" followed by a build date.
	versionLine = regexp.MustCompile(`(?m)^\s*Version:\s*(v?\d+\.\d+\.\d+\S*)\s*$`)
	// `gameap-daemon version` prints "GameAP Daemon version: v4.1.4" followed
	// by the build date and Go details.
	daemonVersionLine = regexp.MustCompile(`(?m)^\s*GameAP Daemon version:\s*(v?\d+\.\d+\.\d+\S*)\s*$`)
	// `gameap --version` prints "GameAP v4.5.7" followed by a build date.
	panelVersionLine = regexp.MustCompile(`(?m)^\s*GameAP\s+(v?\d+\.\d+\.\d+\S*)\s*$`)
)

// ParseVersion reads the result of VersionCommand. exitCode is the command's
// exit status as the daemon reports it.
func ParseVersion(output string, exitCode int32) (string, error) {
	return parseVersion(output, exitCode, "gameapctl", versionLine, ErrNotFound)
}

// ParseDaemonVersion reads the result of DaemonVersionCommand.
func ParseDaemonVersion(output string, exitCode int32) (string, error) {
	return parseVersion(output, exitCode, "gameap-daemon", daemonVersionLine, ErrDaemonNotFound)
}

// ParsePanelVersion reads the result of PanelVersionCommand.
func ParsePanelVersion(output string, exitCode int32) (string, error) {
	return parseVersion(output, exitCode, "gameap", panelVersionLine, ErrPanelNotFound)
}

// IsNotFound reports whether err means the program is not installed on the
// node at all, as opposed to failing to answer.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrDaemonNotFound) || errors.Is(err, ErrPanelNotFound)
}

func parseVersion(output string, exitCode int32, program string, line *regexp.Regexp, errNotFound error) (string, error) {
	if exitCode != 0 {
		if notFound(output, exitCode) {
			return "", errNotFound
		}

		return "", errors.New(program + " exited with status " + strconv.Itoa(int(exitCode)) + ": " + lastLine(output))
	}

	m := line.FindStringSubmatch(output)
	if m == nil {
		return "", ErrUnrecognisedOutput
	}

	return m[1], nil
}

// notFound recognises both launchers' way of saying the program is missing:
// env exits 127, PowerShell exits 1 with a CommandNotFoundException.
func notFound(output string, exitCode int32) bool {
	return exitCode == 127 ||
		strings.Contains(output, "CommandNotFoundException") ||
		strings.Contains(output, "is not recognized as the name of")
}

// ParseLatestRelease reads the tag name out of a GitHub "latest release"
// response body.
func ParseLatestRelease(body []byte) (string, error) {
	tag, _, err := ParseRelease(body)

	return tag, err
}

// ParseRelease reads a GitHub release response body: its tag and the digest
// GitHub lists for each asset ("sha256:<hex>"), keyed by asset name. An asset
// without a digest is left out.
func ParseRelease(body []byte) (string, map[string]string, error) {
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", nil, err
	}

	if release.TagName == "" {
		return "", nil, errors.New("release has no tag_name")
	}

	digests := map[string]string{}
	for _, a := range release.Assets {
		if a.Name != "" && a.Digest != "" {
			digests[a.Name] = a.Digest
		}
	}

	return release.TagName, digests, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}

