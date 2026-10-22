package gameapctl

import (
	"errors"
	"strings"
	"testing"
)

func TestVersionCommandStartsWithAbsoluteLauncher(t *testing.T) {
	for _, build := range []func(string) (string, error){VersionCommand, DaemonVersionCommand, PanelVersionCommand} {
		for _, nodeOS := range []string{"linux", "Linux", "windows"} {
			cmd, err := build(nodeOS)
			if err != nil {
				t.Fatalf("%s: %v", nodeOS, err)
			}

			// The whole point of the launcher: never a bare program name.
			if !strings.HasPrefix(cmd, "/") && !strings.HasPrefix(cmd, `C:\`) {
				t.Errorf("%s: command %q does not start with an absolute path", nodeOS, cmd)
			}
		}
	}
}

func TestDaemonAndPanelVersionCommands(t *testing.T) {
	if got, _ := DaemonVersionCommand("linux"); got != "/usr/bin/env gameap-daemon version" {
		t.Fatalf("got %q", got)
	}

	if got, _ := PanelVersionCommand("linux"); got != "/usr/bin/env gameap --version" {
		t.Fatalf("got %q", got)
	}

	if got, _ := PanelVersionCommand("windows"); !strings.HasSuffix(got, ` -Command "gameap --version"`) {
		t.Fatalf("got %q", got)
	}
}

// Copied from GameAP v4.5.7 in the Ubuntu 26.04 lab node.
func TestParsePanelVersion(t *testing.T) {
	if got, err := ParsePanelVersion("GameAP v4.5.7\nBuild date: 2026-10-15T11:26:40+00:00\n", 0); err != nil || got != "v4.5.7" {
		t.Fatalf("got %q, %v", got, err)
	}

	// The daemon's line must not pass for the panel's.
	if _, err := ParsePanelVersion("GameAP Daemon version: v4.1.4\n", 0); !errors.Is(err, ErrUnrecognisedOutput) {
		t.Fatalf("got %v", err)
	}

	_, err := ParsePanelVersion("/usr/bin/env: 'gameap': No such file or directory\n", 127)
	if !errors.Is(err, ErrPanelNotFound) || !IsNotFound(err) {
		t.Fatalf("got %v", err)
	}

	if _, err := ParsePanelVersion("Usage of gameap:\n", 0); IsNotFound(err) || err == nil {
		t.Fatalf("an older panel's usage text is not a missing panel: %v", err)
	}
}

func TestVersionCommandRejectsUnknownOS(t *testing.T) {
	for _, nodeOS := range []string{"", "darwin", "linux; rm -rf /"} {
		if _, err := VersionCommand(nodeOS); !errors.Is(err, ErrUnsupportedOS) {
			t.Errorf("%q: got %v, want ErrUnsupportedOS", nodeOS, err)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		exitCode int32
		want     string
		wantErr  error
	}{
		{
			name:   "gameapctl v0.34.0 output",
			output: "Version: v0.34.0\nBuild Date: 2026-09-02T15:00:32+00:00\n",
			want:   "v0.34.0",
		},
		{
			name:   "windows line endings",
			output: "Version: v0.34.0\r\nBuild Date: 2026-09-02T15:00:32+00:00\r\n",
			want:   "v0.34.0",
		},
		{
			name:   "pre-release tag",
			output: "Version: v0.35.0-beta1\n",
			want:   "v0.35.0-beta1",
		},
		{
			name:     "env cannot find it",
			output:   "/usr/bin/env: 'gameapctl': No such file or directory\n",
			exitCode: 127,
			wantErr:  ErrNotFound,
		},
		{
			name:     "powershell cannot find it",
			output:   "gameapctl : The term 'gameapctl' is not recognized as the name of a cmdlet\n",
			exitCode: 1,
			wantErr:  ErrNotFound,
		},
		{
			name:    "no version line",
			output:  "something else entirely\n",
			wantErr: ErrUnrecognisedOutput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVersion(tt.output, tt.exitCode)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got error %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

func TestParseVersionOtherFailureKeepsLastLine(t *testing.T) {
	_, err := ParseVersion("starting\npermission denied\n", 2)
	if err == nil || !strings.Contains(err.Error(), "status 2: permission denied") {
		t.Fatalf("got %v", err)
	}
}

// Copied from gameap-daemon v4.1.4 in the Ubuntu 26.04 lab node.
func TestParseDaemonVersion(t *testing.T) {
	out := "GameAP Daemon version: v4.1.4\nBuild Date: 2026-09-09T00:15:12+00:00\nGo OS/Arch: linux amd64\nGo Version: go1.27.0\n"
	if got, err := ParseDaemonVersion(out, 0); err != nil || got != "v4.1.4" {
		t.Fatalf("got %q, %v", got, err)
	}

	// gameapctl's own line must not pass for the daemon's.
	if _, err := ParseDaemonVersion("Version: v0.34.0\n", 0); !errors.Is(err, ErrUnrecognisedOutput) {
		t.Fatalf("got %v", err)
	}

	if _, err := ParseDaemonVersion("/usr/bin/env: 'gameap-daemon': No such file or directory\n", 127); !errors.Is(err, ErrDaemonNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestParseReleaseDigests(t *testing.T) {
	body := []byte(`{"tag_name":"v4.5.8","assets":[` +
		`{"name":"gameap-v4.5.8-linux-amd64.tar.gz","digest":"sha256:2abd"},` +
		`{"name":"no-digest.tar.gz"}]}`)

	tag, digests, err := ParseRelease(body)
	if err != nil || tag != "v4.5.8" {
		t.Fatalf("got %q, %v", tag, err)
	}

	if digests["gameap-v4.5.8-linux-amd64.tar.gz"] != "sha256:2abd" || len(digests) != 1 {
		t.Fatalf("got %v", digests)
	}
}

func TestParseLatestRelease(t *testing.T) {
	got, err := ParseLatestRelease([]byte(`{"tag_name":"v0.34.0","name":"v0.34.0"}`))
	if err != nil || got != "v0.34.0" {
		t.Fatalf("got (%q, %v)", got, err)
	}

	if _, err := ParseLatestRelease([]byte(`{"message":"API rate limit exceeded"}`)); err == nil {
		t.Fatal("want an error for a body with no tag_name")
	}
}
