# GameAP Updater

A GameAP plugin that upgrades the panel, the daemon and gameapctl from the web UI.
It works by running `gameapctl` on each node through the daemon.

Status: an admin page shows installed and latest versions of the panel, each
node's daemon and each node's gameapctl, and can upgrade all three on a Linux
node with systemd. The design is in the "GameAP Update Center: plan" doc.

An upgrade runs `gameapctl self-update`, `gameapctl daemon upgrade` or
`gameapctl panel upgrade` in a transient systemd unit (`gameap-upd-<job id>`),
so it carries on while the daemon or panel that started it restarts. The page
polls the job; it only counts as done once the node reports the target version.
Only upgrades within a major version run; a node already on that version or a
newer one (a pre-release) is left alone.

gameapctl does not verify what it downloads, so every job checks around it.
The plugin reads the SHA-256 digest GitHub lists for the release archive, from
the panel's side. The job (a fixed POSIX `sh` script, `internal/jobs/verify.go`)
downloads that archive on the node from GitHub, cdn.gameap.com or
cdn.gameap.ru (they serve identical archives), compares it with the digest and
hashes the binary inside; gameapctl only runs if it matches, pinned to the
source that served it (`GAMEAP_RELEASE_SOURCE`), and the installed binary must
hash to the verified one afterwards. A source with a different archive is
skipped. It uses curl or wget, and sha256sum, shasum or openssl, whichever the
node has.

- No source served a matching archive: nothing is installed (exit 90).
- The installed binary differs: reported, but it is already installed (exit 91).
- The node cannot verify at all (no downloader or hasher, no source reachable):
  nothing is installed (exit 92), and the page then offers "Update without
  verification" for that job. The backend only accepts `?unverified=1` right
  after such a job; it never skips the check on its own.

The panel can only be upgraded when it runs on one of its own nodes: the plugin
asks every node for a `gameap` binary and offers the upgrade on the one whose
version matches the running panel. gameapctl rolls back the binary and
`config.env` when the new panel fails its health check, but not the database. A daemon upgrade stops the daemon, and its unit uses
`KillMode=control-group`: game servers run by the `tmux` or `simple` process
manager live in that cgroup and should be expected to stop with it (not tested
yet). `systemd`, Docker and Podman servers have their own units or containers.

## Permissions

The plugin requests `node_commands`, which runs commands as root on every node.
Grant it in Administration -> Plugins. GameAP only applies grants when
`PLUGINS_PERMISSIONS_ENFORCE=true` is set in `/etc/gameap/config.env`; it
defaults to `false`, and while it is off every plugin holds every permission.

Every command the plugin sends starts with an absolute, system-owned launcher
(`/usr/bin/env` on Linux, the full `powershell.exe` path on Windows) rather than
a bare program name.

Targets the latest upstream releases: panel v4.5.8, daemon v4.1.5, gameapctl v0.34.0.

## Build

```bash
./build.sh
```

Needs only git and Docker or podman (`CONTAINER=podman` forces podman). It
checks out the GameAP SDK at `SDK_REF` (default `v4.5.8`) into `.sdk/gameap`,
builds the frontend in `node:24-bookworm` and compiles `GameAP-Updater.wasm` with
TinyGo 0.42.0. Install it from Administration -> Plugins -> Upload. The page is
then at `/plugins/amfatvdydz4ym/`.

Unit tests cover the command templates and output parsing in
`internal/gameapctl`, which has no SDK imports:

```bash
./build.sh test
```

## Test lab

The lab runs in local rootless podman, separate from any production panel.

| Node | Image | Runs |
| --- | --- | --- |
| `gameap-node-ubuntu26` | `lab/ubuntu26/Containerfile` (Ubuntu 26.04, systemd) | Panel 4.5.7 + daemon 4.1.5, SQLite |

Build and start the Ubuntu 26.04 node:

```bash
podman build -t localhost/gameap-node-ubuntu26 lab/ubuntu26
podman network create gameap-lab
podman run -d --name gameap-node-ubuntu26 --hostname gameap-node-ubuntu26 \
  --network gameap-lab -p 127.0.0.1:8080:80 --systemd=always \
  localhost/gameap-node-ubuntu26
```

Then install GameAP inside it with gameapctl:

```bash
gameapctl --non-interactive panel install --version 4.5.7 --database sqlite \
  --host 127.0.0.1 --port 80 --with-daemon
```

`--host 127.0.0.1` makes the panel listen on loopback only, for both HTTP and
gRPC (port 31718). Set `HTTP_HOST=0.0.0.0` in `/etc/gameap/config.env` and run
`systemctl restart gameap`. The panel is then at http://localhost:8080.
