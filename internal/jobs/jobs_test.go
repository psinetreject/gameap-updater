package jobs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestNewIDIsValid(t *testing.T) {
	for range 100 {
		id, err := NewID(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}

		if !ValidID(id) {
			t.Fatalf("NewID produced %q, which ValidID rejects", id)
		}
	}
}

func TestNewIDShortRead(t *testing.T) {
	if _, err := NewID(bytes.NewReader([]byte("short"))); err == nil {
		t.Fatal("want an error when the reader runs dry")
	}
}

func TestValidIDRejects(t *testing.T) {
	for _, id := range []string{"", "abc", "ABCDEFGHIJKL", "abcdefghijk;", "abcdefghijkl ", "../etc/passw", "abcdefghijklm"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}

func TestGameapctlSelfUpdateArgs(t *testing.T) {
	args, err := GameapctlSelfUpdateArgs("v0.34.0")
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(args, " "); got != "--non-interactive self-update --version v0.34.0" {
		t.Fatalf("got %q", got)
	}

	for _, bad := range []string{"", "latest", "v0.34.0 --github", "v0.34.0;reboot", "0.34", "$(id)"} {
		if _, err := GameapctlSelfUpdateArgs(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDaemonUpgradeArgs(t *testing.T) {
	args, err := DaemonUpgradeArgs("v4.1.5")
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(args, " "); got != "--non-interactive daemon upgrade --version v4.1.5" {
		t.Fatalf("got %q", got)
	}

	for _, bad := range []string{"", "latest", "v4.1.5 --github", "v4.1.5;reboot"} {
		if _, err := DaemonUpgradeArgs(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPanelUpgradeArgs(t *testing.T) {
	args, err := PanelUpgradeArgs("v4.5.8")
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(args, " "); got != "--non-interactive panel upgrade --version v4.5.8" {
		t.Fatalf("got %q", got)
	}

	if _, err := PanelUpgradeArgs("v4.5.8 --github"); err == nil {
		t.Fatal("accepted an extra flag")
	}
}

func TestCheckUpgrade(t *testing.T) {
	tests := []struct {
		current, target string
		want            error
	}{
		{"v4.5.7", "v4.5.8", nil},
		{"4.1.4", "v4.1.5", nil},
		{"v0.33.0", "v0.34.0", nil},
		{"v4.6.0-dev.1", "v4.6.0", nil},
		{"v4.5.8", "v4.5.8", ErrNotAnUpgrade},
		{"v4.6.0-dev.1", "v4.5.8", ErrNotAnUpgrade},
		{"v4.10.0", "v4.9.9", ErrNotAnUpgrade},
		{"v4.5.8", "v5.0.0", ErrMajorUpgrade},
	}

	for _, tt := range tests {
		if got := CheckUpgrade(tt.current, tt.target); !errors.Is(got, tt.want) {
			t.Errorf("CheckUpgrade(%q, %q) = %v, want %v", tt.current, tt.target, got, tt.want)
		}
	}

	if err := CheckUpgrade("garbage", "v4.5.8"); err == nil {
		t.Error("accepted an unreadable current version")
	}
}

var testVerify, _ = NewVerify("gameap/gameapctl", "gameapctl", "v0.34.0", "amd64",
	"sha256:e842a7f7925856616e11f53a7eafef267bd73541100ccb3babeac72b0ebbd726")

func TestCommandsStartWithEnv(t *testing.T) {
	args, _ := GameapctlSelfUpdateArgs("v0.34.0")
	start, err := StartCommand("abcdefghijkl", testVerify, args)
	if err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{
		DetectSystemdCommand,
		ArchCommand,
		start,
		StatusCommand("abcdefghijkl"),
		LogCommand("abcdefghijkl"),
		StopCommand("abcdefghijkl"),
		ResetFailedCommand("abcdefghijkl"),
	} {
		if !strings.HasPrefix(cmd, "/usr/bin/env ") {
			t.Errorf("%q does not start with /usr/bin/env", cmd)
		}
	}
}

func TestStartCommand(t *testing.T) {
	args, _ := GameapctlSelfUpdateArgs("v0.34.0")
	got, err := StartCommand("abcdefghijkl", testVerify, args)
	if err != nil {
		t.Fatal(err)
	}

	wantPrefix := "/usr/bin/env systemd-run --no-block --quiet --unit=gameap-upd-abcdefghijkl " +
		"--description=gameap-updater-job-abcdefghijkl --property=Type=oneshot --property=RemainAfterExit=yes " +
		"--setenv=HOME /bin/sh -c '"
	wantSuffix := "' gameap-updater " + testVerify.SHA256 + " gameapctl " +
		"github https://github.com/gameap/gameapctl/releases/download/v0.34.0/gameapctl-v0.34.0-linux-amd64.tar.gz " +
		"cdn.gameap.com https://cdn.gameap.com/gameapctl/v0.34.0/gameapctl-v0.34.0-linux-amd64.tar.gz " +
		"cdn.gameap.ru https://cdn.gameap.ru/gameapctl/v0.34.0/gameapctl-v0.34.0-linux-amd64.tar.gz " +
		"-- --non-interactive self-update --version v0.34.0"
	if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("got\n %s", got)
	}

	bad := testVerify
	bad.Sources = []Source{{Name: "github", URL: "https://example.com/x.tar.gz"}}
	if _, err := StartCommand("abcdefghijkl", bad, args); err == nil {
		t.Fatal("accepted a URL outside GameAP's releases")
	}

	bad.Sources = []Source{{Name: "cdn.gameap.com", URL: "https://cdn.gameap.ru/gameapctl/v0.34.0/gameapctl-v0.34.0-linux-amd64.tar.gz"}}
	if _, err := StartCommand("abcdefghijkl", bad, args); err == nil {
		t.Fatal("accepted a URL under another source's name")
	}
}

func TestUnverifiedStartCommand(t *testing.T) {
	args, _ := GameapctlSelfUpdateArgs("v0.34.0")
	got := UnverifiedStartCommand("abcdefghijkl", args)

	want := "/usr/bin/env systemd-run --no-block --quiet --unit=gameap-upd-abcdefghijkl " +
		"--description=gameap-updater-job-abcdefghijkl --property=Type=oneshot --property=RemainAfterExit=yes " +
		"--setenv=HOME /usr/bin/env gameapctl --non-interactive self-update --version v0.34.0"
	if got != want {
		t.Fatalf("got\n %s\nwant\n %s", got, want)
	}
}

// The script travels single-quoted through the daemon's command splitting and
// through systemd, which expands $NAME and %x in ExecStart=.
func TestVerifyScriptSurvivesQuoting(t *testing.T) {
	if strings.ContainsAny(verifyScript, "'%") {
		t.Fatal("verifyScript contains a single quote or a percent sign")
	}

	if escaped := systemdEscape(verifyScript); strings.Count(escaped, "$") != 2*strings.Count(verifyScript, "$") {
		t.Fatal("not every $ was doubled")
	}

	for _, code := range []string{"exit 90", "exit 91", "exit 92"} {
		if !strings.Contains(verifyScript, code) {
			t.Errorf("verifyScript lost %q", code)
		}
	}
}

func TestNewVerify(t *testing.T) {
	v, err := NewVerify("gameap/gameap", "gameap", "v4.5.8", "amd64", "sha256:2abde04e4ed9d765c05c4d8609dd9b074b9451f290336a1e90b0d6748e93e45e")
	if err != nil {
		t.Fatal(err)
	}

	want := []Source{
		{"github", "https://github.com/gameap/gameap/releases/download/v4.5.8/gameap-v4.5.8-linux-amd64.tar.gz"},
		{"cdn.gameap.com", "https://cdn.gameap.com/gameap/v4.5.8/gameap-v4.5.8-linux-amd64.tar.gz"},
		{"cdn.gameap.ru", "https://cdn.gameap.ru/gameap/v4.5.8/gameap-v4.5.8-linux-amd64.tar.gz"},
	}
	if len(v.Sources) != len(want) {
		t.Fatalf("got %+v", v.Sources)
	}
	for i := range want {
		if v.Sources[i] != want[i] {
			t.Errorf("source %d: got %+v, want %+v", i, v.Sources[i], want[i])
		}
	}

	if v.Asset() != "gameap-v4.5.8-linux-amd64.tar.gz" {
		t.Errorf("Asset() = %q", v.Asset())
	}

	for _, bad := range []struct{ repo, binary, digest string }{
		{"evil/gameap", "gameap", "sha256:" + testVerify.SHA256},
		{"gameap/gameap", "sh", "sha256:" + testVerify.SHA256},
		{"gameap/gameap", "gameap", "md5:" + testVerify.SHA256},
		{"gameap/gameap", "gameap", "sha256:" + testVerify.SHA256[:63] + ";"},
	} {
		if _, err := NewVerify(bad.repo, bad.binary, "v4.5.8", "amd64", bad.digest); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64\n": "amd64", "aarch64": "arm64", "i686": "386", "armv7l": "arm", "mips64": "mips64"} {
		if got, err := Arch(in); err != nil || got != want {
			t.Errorf("Arch(%q) = %q, %v", in, got, err)
		}
	}

	if _, err := Arch("riscv64"); !errors.Is(err, ErrUnsupportedArch) {
		t.Errorf("got %v", err)
	}
}

func TestOutcomeNamesChecksumFailures(t *testing.T) {
	for code, want := range map[string]string{
		"90": "did not match the SHA-256",
		"91": "does not match the verified release",
		"92": "could not verify the release",
	} {
		s, _ := ParseUnitStatus("LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainStatus=" + code + "\n")
		if state, why := s.Outcome(); state != StateFailed || !strings.Contains(why, want) {
			t.Errorf("exit %s: got %s %q", code, state, why)
		}
	}
}

// The outputs below are copied from systemd 259 in the Ubuntu 26.04 lab node.
func TestParseUnitStatusOutcome(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   State
	}{
		{"running", "LoadState=loaded\nActiveState=activating\nSubState=start\nResult=success\nExecMainStatus=0\n", StateRunning},
		{"succeeded", "LoadState=loaded\nActiveState=active\nSubState=exited\nResult=success\nExecMainStatus=0\n", StateDone},
		{"failed", "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainStatus=3\n", StateFailed},
		{"gone", "LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nExecMainStatus=0\n", StateFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := ParseUnitStatus(tt.output)
			if err != nil {
				t.Fatal(err)
			}

			if got, _ := s.Outcome(); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseUnitStatusFailureCarriesExitStatus(t *testing.T) {
	s, _ := ParseUnitStatus("LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainStatus=3\n")
	if _, why := s.Outcome(); !strings.Contains(why, "exit status 3") {
		t.Fatalf("got %q", why)
	}
}

func TestParseUnitStatusRejectsGarbage(t *testing.T) {
	for _, out := range []string{"", "Failed to connect to bus\n", "ExecMainStatus=abc\nLoadState=loaded\n"} {
		if _, err := ParseUnitStatus(out); err == nil {
			t.Errorf("accepted %q", out)
		}
	}
}

func TestTail(t *testing.T) {
	if got := Tail("a\nb\nc\n", 2); got != "b\nc" {
		t.Fatalf("got %q", got)
	}

	if got := Tail("a\n", 5); got != "a" {
		t.Fatalf("got %q", got)
	}
}
