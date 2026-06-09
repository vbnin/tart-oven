# Tart Oven - An all-in-one VM management and orchestration server for macOS

A single Go binary that manages, monitors and schedule macOS VMs running
under [Tart](https://tart.run) on a host Mac computer and serves a live web dashboard to
control and monitor them.

This VM orchestration server fully relies on Tart and Apple's virtualization framework.

## What it does

- **Scheduler** — Run random VMs for XX minutes following a schedule and daily working hours.Outside it the scheduler stops all running VMs and starts
  none.
- **SSH status & Info** — on start, after getting an IP, tart-oven runs the
  status command (Get info) over SSH: a green/red bubble shows reachability and
  the **Info** column shows the (multi-line) output. Clicking **Get info**
  refreshes both. Red usually means the key isn't set up.
- **History logs** — every vm run is captured in
  a rolling log for beter visibility.
- **Per-VM actions** — Run, Stop, Restart, Send command (SSH), Get info (SSH
  status command, on demand only), and Screen (open macOS Screen Sharing).
- **VM management** — this server detects Tart installations and can automatically install Tart when missing. It also lets you create/clone/edit/delete VMs.

<img width="1617" height="934" alt="Screenshot 2026-06-09 at 11 54 44" src="https://github.com/user-attachments/assets/d6f0a95e-23e1-4d5a-a058-f93906290b62" />

## WebUI tabs

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

## Build

Two main source files: `main.go` and `index.html` (embedded into the binary with
`//go:embed`).

```sh
go build -o tart-oven
```

That produces one static binary, `tart-oven`. State and configuration lives in
`~/.tart-oven/state.json` (created on first run).

## First Run (Manual mode)

```sh
cd /Path/To/tart-oven # e.g.: cd /Library/Application\ Support/Tart\ Oven 
./tart-oven                      # uses config from state.json (default 127.0.0.1:8080)
./tart-oven -listen 0.0.0.0:9000 # override the bind address
```

## Packaging as a .pkg (for sharing / Jamf deployment)

A single command builds an installer package:

```sh
./packaging/build-pkg.sh                 # → TartOven-<version>.pkg
```

The .pkg installs:
- `/Library/Application Support/Tart Oven/tart-oven` — the binary
- `/Library/LaunchAgents/com.tartoven.agent.plist` — the auto-start agent

…and its **postinstall** loads the agent in the logged-in user's GUI session and
opens `http://127.0.0.1:8080` in the default browser. Double click on the .PKG file 
or install it with:

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
signing certificate) instead of using `SIGN_IDENTITY`.

## Access from another machine on the LAN

It binds to `127.0.0.1:8080` by default (localhost only). To reach it from
another machine, set **Listen** to `0.0.0.0:8080` in the Configuration tab (or
`-listen 0.0.0.0:8080`) and restart, then from your MacBook:

```
http://<host-mac-ip>:8080
```

### Optional auth

Leave the **Bearer token** field empty for no auth (default). Set it to require
`Authorization: Bearer <token>` on all API/SSE calls. The dashboard prompts for
the token on a 401 and keeps it in memory for the session. (The dashboard HTML
itself is always served so the page can load and prompt; only the API and the
event stream are protected.)

### Screen Sharing

The per-VM **Screen** button opens `vnc://admin@<vm-ip>` — i.e. it launches
macOS Screen Sharing **on the computer viewing the dashboard** (your MacBook),
connecting directly to the bridged VM over the LAN. This is independent of how
the VM was started. The guest must have **Screen Sharing / Remote Management**
enabled in its Sharing settings.

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
