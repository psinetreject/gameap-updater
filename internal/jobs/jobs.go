// Package jobs describes an upgrade job and builds the fixed commands that
// start, watch and clean one up on a node. Nothing here talks to the panel, so
// it is tested with plain `go test ./internal/...`.
//
// A job runs as a transient systemd unit so it is detached from the daemon: an
// upgrade can restart the daemon (or the panel) that started it, and the unit,
// owned by systemd, carries on. Nodes without systemd are not supported yet.
package jobs

import (
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// State is where a job stands, as the plugin records it.
type State string

const (
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Job is one upgrade of one component on one node, stored as JSON in plugin
// storage under "job:<id>".
type Job struct {
	ID         string `json:"id"`
	Component  string `json:"component"`
	NodeID     uint64 `json:"node_id"`
	From       string `json:"from"`
	To         string `json:"to"`
	State      State  `json:"state"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	ActorID    uint64 `json:"actor_id,omitempty"`
	Error      string `json:"error,omitempty"`
	Log        string `json:"log,omitempty"`
	// The release archive the job checks, and the SHA-256 GitHub lists for
	// it. A job that is done has matched both the archive and the installed
	// binary against it.
	Asset         string `json:"asset,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	// Unverified is set on a job an admin started without checking;
	// CannotVerify on a job that stopped because the node could not check.
	Unverified   bool `json:"unverified,omitempty"`
	CannotVerify bool `json:"cannot_verify,omitempty"`
}

// Terminal reports whether the job has finished, either way.
func (j *Job) Terminal() bool {
	return j.State == StateDone || j.State == StateFailed
}

// The components a job can upgrade, all of them by gameapctl.
const (
	ComponentGameapctl = "gameapctl"
	ComponentDaemon    = "daemon"
	ComponentPanel     = "panel"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var idPattern = regexp.MustCompile(`^[a-z0-9]{12}$`)

// NewID returns a 12-character job ID made from the first 12 bytes of r. The
// ID ends up in a systemd unit name, so it is restricted to [a-z0-9]. It only
// has to be unique, not secret; see newJobID in the plugin for where the bytes
// come from.
func NewID(r io.Reader) (string, error) {
	buf := make([]byte, 12)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}

	// b % 36 favours a few letters slightly; the ID is a name, not a secret.
	for i, b := range buf {
		buf[i] = idAlphabet[int(b)%len(idAlphabet)]
	}

	return string(buf), nil
}

// ValidID reports whether id has the shape NewID produces. Every ID read from a
// request goes through it before it reaches a command or a storage key.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// A release tag as GitHub and gameapctl spell it. Checked before it reaches a
// command, although it only ever comes from the release feed.
var versionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// ErrBadVersion means a target version is not a plain release tag.
var ErrBadVersion = errors.New("not a release tag")

// GameapctlSelfUpdateArgs returns the gameapctl arguments that update gameapctl
// itself to target.
func GameapctlSelfUpdateArgs(target string) ([]string, error) {
	if !versionPattern.MatchString(target) {
		return nil, ErrBadVersion
	}

	return []string{"--non-interactive", "self-update", "--version", target}, nil
}

// DaemonUpgradeArgs returns the gameapctl arguments that upgrade gameap-daemon
// to target. gameapctl stops the daemon, swaps the binary and starts it again,
// putting the old binary back if the new one does not start; the daemon that
// launched the job is among what it stops, which is why jobs run detached.
func DaemonUpgradeArgs(target string) ([]string, error) {
	if !versionPattern.MatchString(target) {
		return nil, ErrBadVersion
	}

	return []string{"--non-interactive", "daemon", "upgrade", "--version", target}, nil
}

// PanelUpgradeArgs returns the gameapctl arguments that upgrade the GameAP
// panel to target. gameapctl downloads the release, stops the panel, swaps the
// binary, migrates config.env and starts it again; when the new panel fails
// its health check it puts the old binary and config.env back. The database
// is not part of that rollback.
func PanelUpgradeArgs(target string) ([]string, error) {
	if !versionPattern.MatchString(target) {
		return nil, ErrBadVersion
	}

	return []string{"--non-interactive", "panel", "upgrade", "--version", target}, nil
}

// ErrNotAnUpgrade means a node already has the target or something newer.
var ErrNotAnUpgrade = errors.New("not an upgrade")

// ErrMajorUpgrade means the target is in another major version, which is left
// to an operator running gameapctl by hand.
var ErrMajorUpgrade = errors.New("major version change")

// CheckUpgrade says whether moving from current to target is an upgrade a job
// may run: target must be newer, in the same major version. A node on a
// pre-release newer than the latest stable release is left alone.
func CheckUpgrade(current, target string) error {
	c, ok := parseVersion(current)
	if !ok {
		return errors.New("cannot compare version " + strconv.Quote(current))
	}

	t, ok := parseVersion(target)
	if !ok {
		return errors.New("cannot compare version " + strconv.Quote(target))
	}

	if c.nums[0] != t.nums[0] {
		return ErrMajorUpgrade
	}

	if compareVersions(t, c) <= 0 {
		return ErrNotAnUpgrade
	}

	return nil
}

type parsedVersion struct {
	nums [3]int
	pre  string
}

func parseVersion(v string) (parsedVersion, bool) {
	var p parsedVersion

	if !versionPattern.MatchString(v) {
		return p, false
	}

	core, pre, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	p.pre = pre

	for i, part := range strings.Split(core, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return p, false
		}
		p.nums[i] = n
	}

	return p, true
}

// compareVersions orders by major, minor and patch; at equal numbers a release
// comes after its pre-releases, and pre-releases compare as plain strings,
// which is enough for gameap's "-dev.N" and "-beta.N" tags below ten.
func compareVersions(a, b parsedVersion) int {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			if a.nums[i] < b.nums[i] {
				return -1
			}

			return 1
		}
	}

	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	default:
		return strings.Compare(a.pre, b.pre)
	}
}

// Every command starts with /usr/bin/env, never a bare name. See
// internal/gameapctl.
const env = "/usr/bin/env"

// DetectSystemdCommand exits 0 only on a node booted with systemd. It is the
// check systemd itself documents (sd_booted).
const DetectSystemdCommand = env + " test -d /run/systemd/system"

// UnitName is the transient unit a job runs in.
func UnitName(id string) string {
	return "gameap-upd-" + id
}

// StartCommand starts gameapctl with args in a detached transient unit, inside
// verifyScript (see verify.go), which checks the release v describes around
// the gameapctl run. args must come from a function in this package: they are
// joined with spaces, unquoted.
func StartCommand(id string, v Verify, args []string) (string, error) {
	if !v.valid() {
		return "", ErrBadVerify
	}

	words := []string{"/bin/sh", "-c", "'" + systemdEscape(verifyScript) + "'", "gameap-updater", v.SHA256, v.Binary}
	for _, src := range v.Sources {
		words = append(words, src.Name, src.URL)
	}

	words = append(words, "--")

	return unitCommand(id, words, args), nil
}

// UnverifiedStartCommand starts gameapctl with args the way it runs by hand,
// checking nothing. It is only sent when an admin asked for it after a job
// reported that the node cannot verify releases.
func UnverifiedStartCommand(id string, args []string) string {
	return unitCommand(id, []string{env, "gameapctl"}, args)
}

// unitCommand runs program, then args, in a detached transient unit.
//
// Type=oneshot with RemainAfterExit keeps the unit loaded after it exits, so
// its result and exit status can still be read; --no-block returns as soon as
// the unit is queued instead of waiting for the job, which would otherwise
// hold the node command open for the whole upgrade. A transient unit starts
// without HOME, and gameapctl keeps its install state (the daemon's scope,
// whether it was built from GitHub) under $HOME/.gameapctl, so
// --setenv=HOME, with no value, copies the daemon's own HOME into the unit.
func unitCommand(id string, program, args []string) string {
	return strings.Join(append([]string{
		env, "systemd-run",
		"--no-block",
		"--quiet",
		"--unit=" + UnitName(id),
		"--description=gameap-updater-job-" + id,
		"--property=Type=oneshot",
		"--property=RemainAfterExit=yes",
		"--setenv=HOME",
	}, program...), " ") + " " + strings.Join(args, " ")
}

// StatusCommand prints the unit properties ParseUnitStatus reads.
func StatusCommand(id string) string {
	return env + " systemctl show " + UnitName(id) +
		" --property=LoadState,ActiveState,SubState,Result,ExecMainStatus"
}

// LogCommand prints the last lines the job wrote, systemd's own lines included.
func LogCommand(id string) string {
	return env + " journalctl --unit=" + UnitName(id) + " --output=cat --no-pager --lines=200"
}

// StopCommand unloads a unit that finished successfully (it stays "active"
// because of RemainAfterExit).
func StopCommand(id string) string {
	return env + " systemctl stop " + UnitName(id)
}

// ResetFailedCommand unloads a unit that failed.
func ResetFailedCommand(id string) string {
	return env + " systemctl reset-failed " + UnitName(id)
}

// UnitStatus is what `systemctl show` reports for a job's unit.
type UnitStatus struct {
	LoadState   string
	ActiveState string
	SubState    string
	Result      string
	ExitStatus  int
}

// ParseUnitStatus reads StatusCommand's KEY=value lines.
func ParseUnitStatus(output string) (UnitStatus, error) {
	var s UnitStatus

	seen := 0
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}

		seen++
		switch key {
		case "LoadState":
			s.LoadState = value
		case "ActiveState":
			s.ActiveState = value
		case "SubState":
			s.SubState = value
		case "Result":
			s.Result = value
		case "ExecMainStatus":
			n, err := strconv.Atoi(value)
			if err != nil {
				return s, errors.New("unexpected ExecMainStatus " + strconv.Quote(value))
			}
			s.ExitStatus = n
		default:
			seen--
		}
	}

	if seen == 0 || s.LoadState == "" {
		return s, errors.New("no unit properties in systemctl output")
	}

	return s, nil
}

// Outcome says what a unit's status means for the job: still running, finished
// cleanly, or failed (with why).
func (s UnitStatus) Outcome() (State, string) {
	switch {
	case s.LoadState == "not-found":
		return StateFailed, "the job's unit is gone; it may have been cleaned up by hand"
	case s.ActiveState == "activating" || s.ActiveState == "reloading":
		return StateRunning, ""
	case s.ActiveState == "active" && s.Result == "success":
		return StateDone, ""
	case s.ExitStatus == ExitArchiveMismatch:
		return StateFailed, "the downloaded release did not match the SHA-256 GitHub lists for it; nothing was installed"
	case s.ExitStatus == ExitInstalledMismatch:
		return StateFailed, "the installed binary does not match the verified release"
	case s.ExitStatus == ExitCannotVerify:
		return StateFailed, "the node could not verify the release, so nothing was installed (see the log)"
	case s.ActiveState == "failed" || s.Result != "success":
		return StateFailed, "gameapctl failed (" + s.Result + ", exit status " + strconv.Itoa(s.ExitStatus) + ")"
	default:
		return StateRunning, ""
	}
}

// Tail keeps the last n lines of s, which is all a job record stores of a log.
func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}
