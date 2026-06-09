# tart-oven — Tart VM fleet manager

A single Go binary (standard library only) that manages ~20 macOS VMs running
under [Tart](https://tart.run) on a Mac Mini and serves a live web dashboard to
control and monitor them. No framework, no database, no build step beyond
`go build`.

Two source files: `main.go` and `index.html` (embedded into the binary with
`//go:embed`).

## Build

```sh
go build -o tart-oven
```

That produces one static binary, `tart-oven`. State lives in
`~/.tart-oven/state.json` (created on first run).

```sh
./tart-oven                      # uses config from state.json (default 127.0.0.1:8080)
./tart-oven -listen 0.0.0.0:9000 # override the bind address
./tart-oven -state /path/state.json
```

## Install as a LaunchAgent (runs as the logged-in user)

A LaunchAgent runs inside the console user's GUI session, so `tart` sees the
same VMs and **no sudo / user-switching is needed** (unlike the old Jamf-driven
root script).

```sh
# 1. Put the binary somewhere stable
sudo cp tart-oven /usr/local/bin/tart-oven
sudo chmod +x /usr/local/bin/tart-oven

# 2. Install the agent for the *currently logged-in* user
cp com.user.tart-oven.plist ~/Library/LaunchAgents/com.user.tart-oven.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.user.tart-oven.plist

# 3. (optional) start it now without re-login
launchctl kickstart -k gui/$(id -u)/com.user.tart-oven
```

Uninstall / reload:

```sh
launchctl bootout gui/$(id -u)/com.user.tart-oven          # stop & unload
# edit plist or replace binary, then bootstrap again:
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.user.tart-oven.plist
```

Logs: `/Users/Shared/tart-oven.out.log` and `tart-oven.err.log` (change the paths
in the plist if you prefer `~/Library/Logs`).

## Packaging as a .pkg (for sharing / Jamf)

A single command builds an installer package:

```sh
./packaging/build-pkg.sh                 # → TartOven-<version>.pkg
```

The .pkg installs:
- `/Library/Application Support/Tart Oven/tart-oven` — the binary
- `/Library/LaunchAgents/com.tartoven.agent.plist` — the auto-start agent

…and its **postinstall** loads the agent in the logged-in user's GUI session and
opens `http://127.0.0.1:8080` in the default browser. Install it with:

```sh
sudo installer -pkg TartOven-<version>.pkg -target /
```

Signing (only needed for double-click installs outside Jamf — Jamf installs as
root and bypasses Gatekeeper):

```sh
SIGN_IDENTITY="Developer ID Installer: Your Name (TEAMID)" ./packaging/build-pkg.sh
# then notarize:  xcrun notarytool submit … && xcrun stapler staple TartOven-*.pkg
```

You can also sign the produced .pkg in **Jamf Composer** (Build As → choose your
signing certificate) instead of using `SIGN_IDENTITY`. The binary is built for
Apple Silicon (arm64), which is what Tart requires anyway.

## Access from another machine on the LAN

It binds to `127.0.0.1:8080` by default (localhost only). To reach it from
another machine, set **Listen** to `0.0.0.0:8080` in the Configuration tab (or
`-listen 0.0.0.0:8080`) and restart, then from your MacBook:

```
http://<mac-mini-ip>:8080
```

Find the Mini's IP with `ipconfig getifaddr en0` on the Mini. Bind to all
interfaces only on a trusted network — consider setting a bearer token too.

### Optional auth

Leave the **Bearer token** field empty for no auth (default). Set it to require
`Authorization: Bearer <token>` on all API/SSE calls. The dashboard prompts for
the token on a 401 and keeps it in memory for the session. (The dashboard HTML
itself is always served so the page can load and prompt; only the API and the
event stream are protected.)

## What it does

- **Storage banner** — green/red, reflecting whether the configured
  `vmStoragePath` (TART_HOME) is mounted. If it's missing, the scheduler pauses
  automatic runs and the dashboard says so prominently.
- **Scheduler** — every *interval* minutes: stop any VM whose *run window*
  expired, then if running count < *max concurrent*, start a random stopped VM
  that isn't a `TEMPLATE` and isn't excluded, for the configured window.
- **Daily active hours** — optionally gate auto-runs to a window (default
  **08:00–22:00**). Outside it the scheduler stops all running VMs and starts
  none; the dashboard shows "Outside active hours". Windows may wrap midnight.
- **SSH status & Info** — on start, after getting an IP, tart-oven runs the
  status command (Get info) over SSH: a green/red bubble shows reachability and
  the **Info** column shows the (multi-line) output. Clicking **Get info**
  refreshes both. Red usually means the key isn't set up — see the guide.
- **Tart logs** — every tart command (and its stderr on failure) is captured in
  a rolling log shown on the Dashboard. `tart run` output is captured too, so a
  boot failure shows tart's actual error in the log, the VM's "boot failure"
  tooltip, and a toast — instead of just "no IP".
- **Live UI** — `/events` (SSE) pushes the whole state on every change; no
  polling. A 10s monitor keeps states/timers fresh and reconciles against
  `tart list` (Tart is the source of truth), so a VM that tart stopped on its
  own shows as stopped within ~10s. It also heals ops stuck "busy" (a hung tart
  or a daemon restart mid-op) after ~2 min, and tart commands have hard timeouts
  so they can't wedge a VM in "starting"/"stopping". The Dashboard's **Refresh
  VM status** button forces this check on demand.
- **Per-VM actions** — Run, Stop, Restart, Send command (SSH), Get info (SSH
  status command, on demand only), and Screen (open macOS Screen Sharing).
- **Custom run arguments** — a config field appended to every `tart run`
  (e.g. `--vnc --no-audio --net-host`). Quote values with spaces, e.g.
  `--net-bridged="Wi-Fi"`. See `tart run --help` for the full list.

### Screen Sharing

The per-VM **Screen** button opens `vnc://admin@<vm-ip>` — i.e. it launches
macOS Screen Sharing **on the computer viewing the dashboard** (your MacBook),
connecting directly to the bridged VM over the LAN. This is independent of how
the VM was started. The guest must have **Screen Sharing / Remote Management**
enabled in its Sharing settings.

(`tart run --vnc`, by contrast, opens Screen Sharing on the *Mac Mini* host. Add
it via Custom run arguments only if that's what you want.)

## HTTP API

| Method | Path | Body / query | Purpose |
|---|---|---|---|
| GET  | `/api/vms`    | — | full state (VMs + config + storage) |
| POST | `/api/run`    | `{name}` | run a VM now |
| POST | `/api/stop`   | `{name}` | stop a VM now |
| POST | `/api/restart`| `{name}` | stop then run |
| POST | `/api/exec`   | `{name,command}` | SSH exec, returns stdout/stderr/exit |
| GET  | `/api/info`   | `?name=` | SSH status command output |
| GET  | `/api/history`| — | run history (newest first) + retention |
| POST | `/api/vm/create` | createReq JSON | clone from template or create from IPSW (async tasks) |
| POST | `/api/vm/set`    | `{name,cpu,memory,diskSize,display,randomMac,randomSerial}` | `tart set` (VM must be stopped) |
| POST | `/api/vm/rename` | `{name,newName}` | `tart rename` (stopped) |
| POST | `/api/vm/delete` | `{name}` | `tart delete` (stopped) |
| GET  | `/api/vm/get`    | `?name=` | `tart get --format json` (for the edit form) |
| POST | `/api/install-tart` | — | download latest tart from GitHub → /Applications |
| POST | `/api/server/restart` | — | re-exec the tart-oven process |
| POST | `/api/server/stop` | — | stop the tart-oven process (boots out the agent) |
| GET/POST | `/api/config` | Config JSON | read / update config |
| GET  | `/events`     | — | SSE state stream |
| GET  | `/`           | — | the dashboard |

## Dashboard tabs

- **Dashboard** — scheduler ON/OFF, a **Refresh VM status** button (force a
  re-check against tart and clear any stuck statuses), currently-running VMs
  (with SSH/Info and a Get-info button), an SSH command tool (running VMs only,
  with an optional transient sudo password), the full fleet table, and the Tart
  logs. Last known IP / SSH status / Info are retained after a VM stops.
- **History** — a log entry for every VM start (manual or scheduler), with
  duration, IP and trigger. Searchable by name. Retained for **60 days** by
  default (configurable via `historyDays`); older entries are pruned on save and
  on startup.
- **VM Management** — create/clone VMs (clone a template, or create from an IPSW
  path / "latest"; pick count, CPU, RAM, disk, display, random MAC/serial), edit
  an existing VM's settings (`tart set`), rename, and delete. Long create/clone
  operations run as background tasks with live output in the **Activity** panel.
  Edit/rename/delete require the VM to be stopped. This replaces the old
  `create_vm_script.sh` Jamf workflow.
- **Configuration** — settings grouped into VM Scheduler; Tart Settings (storage
  paths, Tart binary path, custom run arguments, the storage-mount banner, and an
  **Update Tart** button showing the installed Tart version); SSH & Commands; and
  Server Settings (listen/bearer token + Restart/Stop server). Also has the SSH
  setup guide.
- **Helper Guide** — this README, rendered in-app.

## Assumptions you may need to correct

These mirror your bash script / the spec. They're constants near the top of
`main.go` (or runtime config in the dashboard) so they're easy to change:

- **Tart binary** — the configurable **Tart binary path** (default
  `/Applications/tart.app/Contents/MacOS/tart`) is used verbatim — no PATH
  guessing. If that binary is missing, the dashboard shows an **Install Tart**
  banner; you can also **Update Tart** anytime from Configuration → Tart Settings
  (both download the latest release from GitHub — the manual method from tart.run
  — and unpack `tart.app` to the bundle derived from that binary path). The
  installed version (`tart --version`) is shown next to the button. Writing under
  `/Applications` needs the console user to be an admin (the usual case);
  otherwise the install task reports a permission error.
- **`tart list --format json`** — detected at startup. If your Tart supports it,
  it's used; otherwise the table output is parsed (best-effort: columns
  `Source Name … State`). Check the startup log line "tart JSON list support".
- **Storage paths** — default `/Volumes/EXTERNAL_SSD/Tart`, fallback
  `/Users/Shared/Tart`. Edit the `defaultStoragePath` constant if your SSD
  mounts elsewhere; it's also editable live in the dashboard.
- **Shared dir** — `/Users/Shared/Tart/Resources`, bind-mounted as
  `host_resources` (editable).
- **Networking** — VMs are started with `--net-bridged=<first active interface>`,
  where the active interface is found exactly like your script (walk
  `networksetup -listnetworkserviceorder`, pick the first device whose
  `ifconfig` shows `status: active`).
- **Jamf** — `jamf recon` runs after start/stop when the toggle is on. Default
  **off**; turn it on in the dashboard if you're on Jamf.
- **sudo over SSH** — the SSH command block has an optional **sudo password**
  field. It's sent only with that one command (never stored in `state.json`,
  never logged) and cleared after each run, so you're asked again every time.
  The server injects `-S` into each `sudo` in your command and feeds the password
  on stdin, so `sudo` works without a TTY (no reliance on sudo's credential
  cache). Just type the plain command, e.g. `sudo pkill -HUP WindowServer` — do
  not add `-S` yourself. (Passwordless sudo via `/etc/sudoers.d` is still the
  option that needs no password at all.)
- **SSH** — user defaults to `admin`, non-interactive flags
  (`BatchMode=yes`, `StrictHostKeyChecking=no`, `UserKnownHostsFile=/dev/null`,
  `ConnectTimeout`). Set an identity file if you don't use an agent/default key.
- **Stopping a VM** — when SSH works, tart-oven first runs the configurable
  **shutdown command** (`sudo shutdown -h now` by default) for a clean macOS
  shutdown and waits up to **shutdown wait timeout** seconds (default 60). If the
  VM hasn't stopped by then (or SSH isn't available), it falls back to
  `tart stop -t 30`, polls ~30s, then force-kills the `tart run` process.
- **Listen address changes** take effect on restart (we don't rebind a live
  listener); everything else applies immediately.
