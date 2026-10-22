package jobs

import (
	"errors"
	"regexp"
	"strings"
)

// gameapctl downloads releases without checking them, and has no way to
// install an archive someone else downloaded. A job therefore checks around
// it: before gameapctl runs, the node downloads the release archive and
// compares it with the SHA-256 digest GitHub lists for it (fetched by the
// plugin, on the panel's side, so the node's network path cannot change it),
// and hashes the binary inside. gameapctl is then pinned to the source that
// served the matching archive (GAMEAP_RELEASE_SOURCE), and afterwards the
// installed binary has to hash to the verified one.
//
// The archive may come from any of gameapctl's three sources: GitHub and both
// GameAP CDNs serve byte-identical archives, so only one of them has to be
// reachable. A source whose archive does not match is skipped.
//
// A mismatched archive stops the job before anything is installed. A
// mismatched installed binary can only be reported: by then gameapctl has
// replaced the old one. When the node cannot check at all (no downloader or
// hasher, or no source reachable) the job stops too, and upgrading anyway is
// a separate, explicit request (UnverifiedStartCommand): falling back on its
// own would let anyone who can block the downloads skip the check.

// Exit statuses of verifyScript, chosen clear of curl's, tar's and gameapctl's.
const (
	ExitArchiveMismatch   = 90
	ExitInstalledMismatch = 91
	ExitCannotVerify      = 92
)

// ArchCommand prints the node's machine hardware name.
const ArchCommand = env + " uname -m"

// ErrUnsupportedArch means GameAP publishes no build for the node's CPU.
var ErrUnsupportedArch = errors.New("no GameAP build for this CPU")

// Arch maps `uname -m` to the architecture in GameAP's release asset names.
// Not every component is built for all of them (gameapctl has no arm or mips
// build); a missing asset is caught when the release is looked up.
func Arch(unameOutput string) (string, error) {
	switch m := strings.TrimSpace(unameOutput); m {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "i386", "i486", "i586", "i686":
		return "386", nil
	case "armv6l", "armv7l":
		return "arm", nil
	case "mips", "mips64":
		return m, nil
	default:
		return "", ErrUnsupportedArch
	}
}

// AssetName is the release archive of binary at tag for linux/arch, as all
// three GameAP projects name them ("gameap-daemon-v4.1.5-linux-amd64.tar.gz").
func AssetName(binary, tag, arch string) string {
	return binary + "-" + tag + "-linux-" + arch + ".tar.gz"
}

// Source is one place a release archive can be downloaded from, named as
// gameapctl names it for GAMEAP_RELEASE_SOURCE.
type Source struct {
	Name string
	URL  string
}

// Verify describes the release archive a job checks before and after
// gameapctl runs.
type Verify struct {
	SHA256  string
	Binary  string
	Sources []Source
}

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// Each source only for GameAP's own projects, a release tag and a plain
	// file name.
	sourcePatterns = map[string]*regexp.Regexp{
		"github": regexp.MustCompile(
			`^https://github\.com/gameap/(gameap|daemon|gameapctl)/releases/download/` + tagAndFile),
		"cdn.gameap.com": regexp.MustCompile(`^https://cdn\.gameap\.com/(gameap|gameap-daemon|gameapctl)/` + tagAndFile),
		"cdn.gameap.ru":  regexp.MustCompile(`^https://cdn\.gameap\.ru/(gameap|gameap-daemon|gameapctl)/` + tagAndFile),
	}
	binaries = map[string]bool{"gameap": true, "gameap-daemon": true, "gameapctl": true}
)

const tagAndFile = `v?\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?/[0-9A-Za-z._-]+\.tar\.gz$`

// ErrBadVerify means a Verify would put something other than GameAP release
// URLs, a SHA-256 hex digest or a known binary name into a command.
var ErrBadVerify = errors.New("bad release verification details")

// NewVerify builds the Verify for binary's archive in repo at tag, with the
// digest GitHub lists for it ("sha256:<hex>"). The sources are tried in the
// order gameapctl falls back through them when it is not pinned.
func NewVerify(repo, binary, tag, arch, digest string) (Verify, error) {
	asset := AssetName(binary, tag, arch)
	cdnPath := "/" + binary + "/" + tag + "/" + asset

	v := Verify{
		SHA256: strings.TrimPrefix(digest, "sha256:"),
		Binary: binary,
		Sources: []Source{
			{Name: "github", URL: "https://github.com/" + repo + "/releases/download/" + tag + "/" + asset},
			{Name: "cdn.gameap.com", URL: "https://cdn.gameap.com" + cdnPath},
			{Name: "cdn.gameap.ru", URL: "https://cdn.gameap.ru" + cdnPath},
		},
	}

	if !strings.HasPrefix(digest, "sha256:") || !v.valid() {
		return Verify{}, ErrBadVerify
	}

	return v, nil
}

// Asset is the archive's file name.
func (v Verify) Asset() string {
	if len(v.Sources) == 0 {
		return ""
	}

	u := v.Sources[0].URL

	return u[strings.LastIndex(u, "/")+1:]
}

func (v Verify) valid() bool {
	if !sha256Pattern.MatchString(v.SHA256) || !binaries[v.Binary] || len(v.Sources) == 0 {
		return false
	}

	for _, src := range v.Sources {
		pattern, ok := sourcePatterns[src.Name]
		if !ok || !pattern.MatchString(src.URL) {
			return false
		}
	}

	return true
}

// verifyScript runs in the job's unit as `sh -c verifyScript gameap-updater
// <sha256> <binary> <name> <url> [<name> <url>...] -- <gameapctl args...>`.
// It must not contain a single quote (it travels single-quoted through the
// daemon's command splitting) or a percent sign, and every $ is doubled for
// systemd, which expands $NAME in ExecStart= itself. It sticks to POSIX sh
// and uses whichever downloader and hasher the node has.
var verifyScript = strings.Join([]string{
	`set -eu`,
	`want="$1"; bin="$2"; shift 2`,
	`if command -v curl >/dev/null 2>&1; then fetch() { curl -fsSL --proto =https --connect-timeout 15 --retry 2 -o "$1" "$2"; }`,
	`elif command -v wget >/dev/null 2>&1; then fetch() { wget -q -T 30 -O "$1" "$2"; }`,
	`else echo "Cannot verify the release: neither curl nor wget is installed."; exit 92; fi`,
	`if command -v sha256sum >/dev/null 2>&1; then sum256() { sha256sum "$1" | cut -d " " -f 1; }`,
	`elif command -v shasum >/dev/null 2>&1; then sum256() { shasum -a 256 "$1" | cut -d " " -f 1; }`,
	`elif command -v openssl >/dev/null 2>&1; then sum256() { openssl dgst -sha256 -r "$1" | cut -d " " -f 1; }`,
	`else echo "Cannot verify the release: none of sha256sum, shasum or openssl is installed."; exit 92; fi`,
	`if ! command -v tar >/dev/null 2>&1; then echo "Cannot verify the release: tar is not installed."; exit 92; fi`,
	`dir="$(mktemp -d /var/tmp/gameap-updater.XXXXXXXXXX)"`,
	`trap "rm -rf $dir" EXIT`,
	`source=""; mismatch=""`,
	`while [ "$1" != "--" ]; do`,
	`  name="$1"; url="$2"; shift 2`,
	`  if [ -n "$source" ]; then continue; fi`,
	`  echo "Downloading $url"`,
	`  if ! fetch "$dir/release.tar.gz" "$url"; then echo "Could not download from $name"; continue; fi`,
	`  got="$(sum256 "$dir/release.tar.gz")"`,
	`  if [ "$got" = "$want" ]; then source="$name"; echo "Archive from $name verified: sha256 $got"`,
	`  else mismatch="$mismatch $name"; echo "WARNING: the archive from $name is $got, GitHub lists $want. Skipping $name."; fi`,
	`done`,
	`shift`,
	`if [ -z "$source" ] && [ -n "$mismatch" ]; then echo "No source served the archive GitHub lists (mismatch from:$mismatch). Nothing was installed."; exit 90; fi`,
	`if [ -z "$source" ]; then echo "Cannot verify the release: no source could be reached."; exit 92; fi`,
	`tar -xzf "$dir/release.tar.gz" -C "$dir" "$bin"`,
	`binsum="$(sum256 "$dir/$bin")"`,
	`echo "Release $bin: sha256 $binsum"`,
	`GAMEAP_RELEASE_SOURCE="$source" gameapctl "$@"`,
	`path="$(command -v "$bin")"`,
	`now="$(sum256 "$path")"`,
	`if [ "$now" != "$binsum" ]; then echo "The installed $path (sha256 $now) does not match the verified release."; exit 91; fi`,
	`echo "Installed $path matches the verified release."`,
}, "\n")

// systemdEscape doubles every $ so systemd passes the script to sh unchanged.
func systemdEscape(s string) string {
	return strings.ReplaceAll(s, "$", "$$")
}
