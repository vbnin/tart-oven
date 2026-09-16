# Changelog

What changed in each release of Tart Oven, newest first.

A few terms appear throughout:

- **Guest** — a virtual machine running under Tart Oven.
- **OCI image** — a macOS image downloaded from a registry. You pull it once and
  clone local VMs from it; you don't run the image itself.
- **Guest agent** — a small helper that ships inside the official macOS images. It
  lets Tart Oven run commands inside a guest without SSH, keys, or guest networking.
- **MDM / Jamf Pro** — the system that manages enrolled Macs. Tart Oven can prepare
  a VM for enrollment and report whether a guest is enrolled.

## 1.53 (2026-09-16)

### Added

- Guest provisioning for macOS 27+ in Create from IPSW. Seeds a fresh guest's
  account (full name, username, password, auto login, remote login) on first
  boot using `--provisioning-opts`. Per create only, not saved. Skipped for
  clones, OCI pulls, and Linux guests. Requires host macOS 27+ and a tart
  build that supports the flag.

### Fixed

- Auto enroll script failing on macOS 27. The redesigned System Settings
  sidebar broke the click used to find the "+" (add profile) button. Now
  located by on screen position instead of click order, which works on both
  macOS 26 and 27. Also fixed the Enroll/Cancel prompt clicking the wrong
  button.
- Guest agent install failing with "sudo: a password is required". The
  Homebrew formula runs its own internal `sudo` call with no way to supply a
  password. Fixed with a scoped `SUDO_ASKPASS` helper.
- Guest commands taking up to 30 extra seconds to run. Every command tried
  the guest agent first and waited out its connection timeout before falling
  back to SSH. Now checked once per VM boot instead of on every command.
- Auto enroll silently enrolling into the wrong Jamf server. It always pushed
  a brand new profile, defaulting to whichever server was first in
  Configuration, ignoring the profile already deployed on the base VM. Now
  reuses an existing profile on the guest's Desktop if one is present.

### Changed

- Activity log now scrolls as a single list and keeps its scroll position
  between refreshes.
- Guest provisioning fields moved into the Create from IPSW form.
- Cleaned up several outdated helper texts in Configuration and the base VM
  prep flow.

## 1.52 — 2026-09-04

### Auto-enrolling VMs at boot (experimental)

- **Added unattended MDM enrollment.** "Auto Enroll VM" (dashboard row menu)
  pushes a fresh enrollment profile to a guest's Desktop and drives System
  Settings through Install → Enroll entirely over SSH/AppleScript — no Screen
  Sharing, no manual clicks. Runs the VM first if it's stopped; refreshes the
  MDM column once it's done, whichever way it goes. Also handles the full
  first-boot Setup Assistant walkthrough when `--random-serial` gives a clone
  a hardware identity macOS treats as never provisioned (not needed on
  macOS 27+, which skips that walkthrough itself).
- **Added "Auto enroll at first boot"** next to Random serial in
  Clone-from-template: a clone made with it checked runs the same flow on its
  own the next time it boots, scheduler-triggered or manual, with no further
  intervention.
- **Added a base-VM prep step** ("Enable Auto-Enrollment Capabilities on Base
  VM", under Prepare base VM for Jamf) that walks a base VM through the two
  one-time TCC grants this automation depends on, with live Autologin /
  Accessibility / enrollment-profile status pills and an on-VM + in-app
  confirmation once both are granted. One-time per base VM — every clone
  inherits the result.

### Dashboard cleanup

- Collapsed Get info, Install Agent, Start Screen Sharing, and Edit Tags into
  a "⋯" menu next to Run/Stop, freeing up width for the SSH and Info columns.
  Added Edit VM and Delete VM shortcuts to the same menu. Install Agent no
  longer waits for a failed check to appear.
- Fixed the menu closing itself a few seconds after opening (it was being
  wiped by the periodic dashboard refresh) and being clipped/requiring a
  scroll for the last couple of rows in the table.
- MDM column now shows the console URL without its `https://` prefix.

### Also

- Removed the redundant per-call SSH override fields from "Deploy MDM
  Enrollment Profile" — it already falls back to the target VM's own
  credentials or the Configuration defaults.
- Moved the Setup Wizard panel to the top of Configuration and removed its
  duplicate button from the header bar.

## 1.51 — 2026-08-27

### VM table cleanup

- **Added a green "OK" / red "KO" label** next to the SSH status dot once a
  guest command has actually been checked, so the result reads clearly
  without squinting at a small dot. The checking (amber, blinking) and
  stopped (grey) dots are unchanged.
- **Removed the separate "SSH"/"agent" pill** next to the status dot in the
  local VM table. Which transport last answered is still available, now as
  the dot's tooltip, instead of a permanent badge crowding the row.
- **Fixed a double-scrollbar glitch in the Info column**: a long unbroken
  token (e.g. an SSH error like `(publickey,password,keyboard-interactive)`)
  could overflow the box horizontally instead of wrapping, showing both a
  horizontal and a vertical scrollbar at once. The box now wraps long tokens
  and only ever scrolls vertically.

### Create / clone VMs: OCI pull moved inline

- **Pulling a base image is now a third option** next to "Clone from template"
  and "Create from IPSW" in Create / clone VMs, styled as one three-button
  group instead of two radio buttons plus a separate, disconnected button.
- **No more popup for this flow**: picking "Pull OCI Image" shows the preset
  chips, registry field, and live pull progress directly in the section: the
  "Pull OCI Image" modal itself is unchanged and still used by the OCI Images
  panel and the first-run empty-state hero card.
- Dropped the decorative down-arrow from this button (the OCI Images panel
  and empty-state hero buttons keep it) and gave the three mode buttons a
  fixed height, so switching between them no longer shifts the row's size.

### Header

- **No more empty pill when the server label is unset.** The label badge now
  hides itself entirely instead of showing as a small blank oval next to the
  host stats.

### Configurable graceful shutdown

- **New toggle in Configuration → SSH & Commands: "Prioritize shutdown using
  SSH".** Off (default) keeps the fast `tart stop` behavior introduced in 1.50.
  On, stopping a VM first asks the guest to shut down cleanly over SSH
  (`sudo shutdown -h now`), waiting up to 30 seconds before falling back to the
  standard stop — a safer option for guests that need time to flush state on
  power-off.

## 1.50 — 2026-08-25

First-time setup is now guided, you can download images without leaving the app, and
the built-in guide has been rewritten.

### Setup wizard

- **A five-step wizard** checks your Mac and Tart installation, confirms where VMs
  will be stored, helps you download a base image, applies settings that match how
  you work, and starts your first VM.
- **A friendlier empty dashboard**: with no local VMs yet, you get a card with
  **Launch Setup Wizard** and **Pull Base Image** instead of an empty table.
- **Three presets**, each a handful of sensible defaults you can change afterwards:
  **DevOps / CI** (headless, no audio, one VM at a time), **Jamf / Mac admin**
  (random serial and MAC, Jamf recon, MDM column on), and **QA tester** (screen and
  audio on).
- **Reopen it any time** from **Setup Wizard** in the header or in **Configuration**.
  Tart Oven remembers that you've finished it once.

### Pull images from the dashboard

- **Pull OCI Image** opens a dialog with one-click choices for macOS 26 (Tahoe),
  macOS 15 (Sequoia), and macOS 14 (Sonoma), plus a field for any other registry
  address.
- **Downloads run in the background** and stream progress into the dialog. Reloading
  the page or closing the dialog doesn't cancel the download or lose the log.
- **Disk space is checked first**: Tart Oven wants at least 25 GiB free before it
  starts, so a large image can't quietly fill your disk.

### Rewritten Helper Guide

- **Eight stages** covering what Tart Oven is for, a five-minute quickstart, working
  with base images, running a fleet, Jamf and MDM, keeping the host healthy, the HTTP
  API, and six troubleshooting runbooks.
- **The Jamf rule spelled out**: clone with a randomized serial and MAC
  (`tart set <vm> --random-serial --random-mac`), or your VMs will overwrite each
  other's inventory records in Jamf.
- **Easier to read**: a table of contents that follows you down the page, a search
  box, and a copy button on every command.

## 1.40 — 2026-08-24

### MDM enrollment column

- **See enrollment at a glance**: a new **MDM** column reports whether each guest is
  enrolled — green with the Jamf Pro URL beneath it when it is, red when the guest
  reports no enrollment, and grey when it hasn't been checked yet.
- **Grey is not red**: a VM that has never been probed, or whose probe failed, shows
  grey. Reporting an unchecked VM as unenrolled was the exact flaw in the line this
  column replaces.
- **The address you'd actually open**: macOS reports the MDM server as
  `https://your-server/mdm/ServerURL`. The column shows `https://your-server`, with
  the raw value on hover.
- **A reliable source**: enrollment comes from a built-in check with a fixed output
  shape, not from the editable Status command, so editing that field can't break the
  column. It runs wherever **Get info** already runs.

### `(no Jamf user)` removed

The default Status command no longer looks up the Jamf username. That lookup printed
`(no Jamf user)` whenever the read failed for *any* reason — including on VMs that
were enrolled and managed perfectly well but had no username profile scoped to them.
It reported healthy VMs as unmanaged, and answered a question the MDM column now
answers properly.

Existing installs are migrated for you: an untouched default Status command is
replaced. One you've customised is left alone.

### Notes

The **Notes** column made way for MDM. Notes themselves are unchanged and still
editable per VM — they just no longer take up a column.

## 1.39 — 2026-08-24

### Guest agent installer fixes

Testing against a VM that genuinely had no agent turned up two bugs in the installer
shipped in 1.38.

- **"Homebrew is not installed" on guests that have it**: a non-interactive SSH
  session starts with a bare `PATH` that doesn't include Homebrew, so the check
  failed on every real guest. The installer now repairs `PATH` before it looks.
- **Incomplete startup files**: the launchd jobs it wrote were missing `PATH`, the
  agent's working directory, and its log paths. `PATH` matters most — commands you
  run through the agent inherit its environment, so anything called by name could
  fail. The generated files now match the official images exactly.

### Documentation

- The **Helper Guide** and README now present the guest agent as the normal way to
  run guest commands, with SSH as the fallback, instead of walking everyone through
  SSH key setup they probably don't need.
- Documented the **Install agent** action, the **Allow SSH fallback** setting, and
  the requirement that the SSH identity file be a full path (or start with `~/`).
- The API reference lists the endpoints added since it was last revised, and no
  longer describes `/api/exec` as SSH-only.
- Fixed the red status-bubble troubleshooting entry, which told you to check SSH
  credentials for something the guest agent now usually handles.

## 1.38 — 2026-08-24

### SSH is now optional

- **Allow SSH fallback for guest commands**: a new setting under **Configuration →
  SSH & Commands**, on by default. Turn it off and Tart Oven talks to guests only
  through the guest agent — if a guest doesn't answer, you get a clear message saying
  so instead of a silent SSH attempt.
- **The SSH setup guide hides itself** when it can't apply. With the fallback off,
  the **SSH setup guide** panel and the **SSH identity file** field disappear.
  Previously the guide sat in VM Management even for fleets where nothing in it
  applied.
- **Deploy MDM Profile is unaffected**: Jamf enrollment copies a file over SFTP,
  which the guest agent can't do, so that path keeps using SSH either way. The
  default SSH username and password stay visible because it still needs them.

### Install the guest agent from the dashboard

- **Install agent**: a running VM whose commands fell back to SSH now offers an
  **Install agent** action. It installs the agent through Homebrew inside the guest,
  sets up its two startup jobs, and then checks that the agent actually answers
  before reporting success — rather than trusting the install output.
- Progress streams to the **Activity** panel. The action needs the guest's SSH
  password and sudo, because the agent is the very thing that's missing and can't
  install itself. It's meant for preparing a base image before you clone it.

## 1.37 — 2026-08-24

### Guest commands run through the guest agent

- **No SSH required**: **Get info** and **Send command** now run through the guest
  agent over a virtual socket — no SSH, no key, no password, no `authorized_keys`,
  no guest networking. Exit codes, output, and `sudo` behave as before. The official
  `ghcr.io/cirruslabs/macos-*-base` images ship the agent preinstalled.
- **SSH still works**: guests without the agent (`vanilla-*` images, for example)
  fall back to SSH exactly as before. Each VM row shows whether **agent** or **ssh**
  answered, so you can see which path is in use.
- **More reliable IP at boot**: VM addresses are resolved with Tart's `agent`
  resolver first — the one Tart documents as working in all cases, needing no guest
  network traffic — before falling back to matching against the host's ARP table.
- **One dead end removed**: the `dhcp` resolver only works for VMs that aren't
  bridged, and Tart Oven always runs bridged, so it could never succeed. It's gone.

### Automatic SSH key provisioning removed

The feature added in 1.36 has been withdrawn. It existed to work around Tart Oven's
own key-only SSH setting, which the guest agent makes unnecessary. It also had two
bugs worth naming: it re-provisioned every running VM roughly every 40 seconds
instead of once, and it skipped `TEMPLATE` VMs — the exact VMs its own setup guide
told you to use.

The **Install the SSH key on new VMs automatically** setting is gone. Tart Oven still
generates an SSH key when one is missing, for the fallback, but never installs it
into a guest on its own.

### Fixes

- **SSH identity file** is no longer labelled "(optional)" — it's required whenever
  the SSH fallback is used. A blank value is repaired from the default on load, and a
  relative path such as `tart-oven` is rejected instead of being quietly resolved
  against the server's working directory (`/`), where it would have written a private
  key.
- **Editing notes no longer clears a VM's SSH username.** Saving notes from the
  dashboard row used to wipe any custom per-VM username, because the editor doesn't
  include that field. A blank submission now means "leave unchanged", matching how
  the password field already worked.
- **Correct pointer in the guide**: the red status indicator pointed at
  Configuration; the SSH setup guide lives in VM Management.

### Suspend fully removed

The Suspend button went away in 1.34, but the endpoint and the suspended states
stayed behind — so a VM suspended by any means became unusable: no Resume, Stop
disabled, and Edit, Rename, and Delete all refusing. All of it is now gone, and any
VM stuck in that state can be stopped and reused normally.

## 1.36 — 2026-08-24

### Automatic SSH key provisioning (opt-in)

> Withdrawn in 1.37. The guest agent made it unnecessary.

- **Hands-off guest access**: a new **Install the SSH key on new VMs automatically**
  setting. When enabled, Tart Oven installs its public key into each running VM about
  a minute after boot, so **Send command** and **Get info** work without running
  `ssh-copy-id` on every VM.
- **Key generation**: if the configured identity file doesn't exist, Tart Oven
  creates one. An existing key is never overwritten.
- **Never repeats itself**: provisioning is skipped when key login already works, so
  a VM that already accepts the key — including clones that inherited it — is left
  alone. A guest that loses the key gets it back on its next boot.
- **Boot-aware retries**: waits 30 seconds for the guest to finish booting, then
  retries with backoff for up to 10 minutes. A guest that rejects the stored
  credentials stops immediately and reports the failure rather than retrying.
- **Safe by default**: ships disabled, honours the scheduler exclude list, skips
  TEMPLATE VMs and OCI images, adds to `authorized_keys` without disturbing keys the
  guest already trusts, and never sends the private key anywhere.

### Header and theme

- **Theme toggle moved to the header.** Light/dark is a per-browser display
  preference, not a server setting, so it no longer lives in Configuration.
- **Memory pressure at a glance**: the header now shows CPU, RAM, and macOS memory
  pressure. VM disk capacity stays on the **Performance** tab, where its chart gives
  it context.
- **Header and charts can no longer disagree.** They now read the same performance
  sample. Each metric falls back independently when its source is unavailable.
- **Fewer redundant fetches**: the Performance tab re-downloads its 24-hour history
  only when there's actually a new sample.

> **API note:** `/api/vms` and the event stream replace the `hostStats` object with
> `performance`, which carries the full sample. The dashboard is the only consumer of
> this field.

## 1.35 — 2026-08-24

### Security

- **Other websites can't drive your dashboard.** Every endpoint that changes
  something now rejects requests that come from another site. This blocks a malicious
  page open in your browser from quietly running guest commands, deleting VMs, or
  rewriting your config. The dashboard itself and non-browser clients such as `curl`
  are unaffected.
- **Safer Tart install and update.** The updater now replaces a directory only when
  it really is a `tart.app` bundle, so a crafted path can't make it delete something
  else.

### VM lifecycle and SSH

- **Rename keeps per-VM state.** Renaming a VM carries its notes, tags, and SSH
  credentials across instead of silently dropping them, so later SSH and MDM
  operations keep working.
- **Long guest commands are no longer cut off.** **Send command** and **Get info** no
  longer treat the SSH *connect* timeout as an overall deadline, so something slow but
  legitimate — `softwareupdate`, say — can run to completion. A genuinely dead
  connection is still dropped quickly.
- **On-demand IP lookup is as robust as boot.** **Get info**, **Send command**, and
  **Deploy MDM Profile** now use the same resolver chain as VM boot, so they work
  after a restart or for VMs started outside Tart Oven — not just right after boot.

### Jamf and MDM

- **A missing Jamf profile fails loudly.** Deploying with a profile that no longer
  exists — a stale dropdown after it was deleted in another tab — now returns a clear
  error instead of quietly enrolling the VM into the old default server.
- **Legacy invitation codes survive a save.** Saving the Jamf profiles list no longer
  drops an older single-server invitation code.

### Dashboard reliability

- **One broken control can't blank the page.** Dashboard event bindings are guarded,
  so a removed or renamed element can no longer throw during startup and take live
  updates, tabs, and buttons down with it.
- **Filters don't commit unsaved settings.** Toggling **Show running only** or
  **Pause** now sends only the field you changed, instead of committing every
  half-edited Settings field along with it.
- **Dead settings removed.** **Shutdown command (SSH)** and **Shutdown wait timeout**
  stopped doing anything in 1.34, so the UI no longer advertises them.
- **Fixed a rare crash** when the dashboard sent an update while a create or clone
  task was still writing its output.

## 1.34 — 2026-08-24

### Multiple Jamf Pro servers

- **Named server profiles**: keep several Jamf environments — Production, Staging,
  Sandbox — in one list, each with its own name, base URL, and masked invitation
  code.
- **Deploy MDM Profile panel**: pick a running base VM, pick the Jamf server,
  optionally override the guest SSH credentials, and push the generated
  `.mobileconfig` to `~/Desktop/mdm_enroll.mobileconfig` in the guest.
- **Partial saves stopped clobbering other settings.** Updating Jamf or SSH settings
  used to reset fields you hadn't submitted and kick the scheduler. Updates now merge
  only what you actually changed and keep existing secrets.

### Fixes

- **VMs failing to get an IP at boot.** On some Macs the address lookup returned
  nothing because of how macOS formats short values in its ARP table, and boot was
  reported as failed. Tart Oven now handles that format and tries several lookup
  methods in turn.

### Dashboard

- **Simpler row actions**: **Run**, **Stop**, **Get info**, **Screen**, and **Edit**.
  Suspend was removed.
- **Fast stop.** Stop now terminates a VM quickly, with a force-kill fallback, which
  suits short-lived test VMs. If your work is data-sensitive, shut the guest down
  from inside first.
- **Changelog in a popup** — read it without leaving the page. Hello.
- **Signed installer package**: `./packaging/build-pkg.sh` signs the binary and the
  installer and writes the result to `~/Downloads`.
- **A more forgiving dashboard**: missing UI elements no longer interrupt the scripts
  that fill in configuration and scheduler controls.

## 1.33 — 2026-08-24

A maintenance release, mostly under the hood.

- **Stalled guests no longer hang the scheduler.** SSH commands now have a real
  deadline based on your configured SSH timeout, so a guest that crashes or drops off
  the network during a boot probe or command can't stall the loops behind it.
- **Safer MDM file transfers**: remote paths are checked, and `../` can't be used to
  climb out of the destination directory.
- **A failure generating an MDM profile now reports an error** instead of taking the
  process down with it.
- **Log rotation handles errors** rather than failing silently.
- **Rapid restarts are safe.** Restarting a VM in quick succession no longer risks
  losing track of the newly started process.
- **Lighter memory measurement.** The periodic memory check no longer briefly pauses
  the rest of Tart Oven while it runs.

## 1.32 — 2026-08-23

### OCI images and local VMs are now separate

- **Two sections on the Dashboard.** **Local VMs** keep every existing action.
  **OCI Images** show the full registry reference, cached size, virtual disk size,
  and when they were last used — and their only action is **Clone**.
- **Clone knows what you clicked.** Cloning an OCI row opens VM Management in clone
  mode with the exact tag or digest already selected.
- **Search covers both sections.** **Show running only** hides OCI images and shows
  only active local VMs.
- **Local-only actions stay local.** Edit, delete, and Jamf preparation exclude OCI
  entries. The clone-source picker offers both.

### Scheduler

- **OCI images are excluded from scheduling** by default, including on existing
  installs that predate the setting. You can turn that off if you want them back in
  rotation, and the choice survives restarts.
- Per-VM and per-template exclusions work as before.

A note on provenance: Tart reports a clone as a local VM and doesn't say which OCI
image it came from, so neither does Tart Oven.

### API

- `/api/vms` now includes `source`, `disk`, `size`, and `accessed` for entries
  discovered through Tart.

## 1.31 — 2026-08-23

### Memory-pressure safeguards

- **New VMs wait when the host is under critical memory pressure.** This covers both
  scheduled and manual starts. Running VMs are left alone, and the gate clears as soon
  as pressure drops back to warning or normal.
- **A missed reading doesn't unlock the gate.** If a pressure sample can't be taken,
  the last known state stands.
- **The Performance card tells you** when starts are being deferred.
- **No `purge`.** Tart Oven never runs macOS's `purge` command — memory held by
  running VMs is not a stale file cache, and treating it as one doesn't help.
- **The VM editor suggests lowering memory** in small, tested steps when host
  pressure warrants it. Tart Oven never changes VM memory for you; changes apply on
  the next boot.

### Recovery controls

> Suspend was removed again in 1.34 and fully cleaned up in 1.37.

- Added per-VM **Suspend** and **Graceful shutdown** actions.
- **Graceful shutdown** sends the configured shutdown command, waits for the guest to
  stop, and leaves it running with a visible error if it can't confirm. It never kills
  the VM behind your back.
- **Stop is unchanged**, including its fallbacks.
- Suspended VMs are protected from configuration, rename, and delete until they're
  resumed and stopped normally.

## 1.30 — 2026-08-22

### Host performance monitoring

- **A new Performance tab** with current values and charts for host CPU, physical
  memory in use, macOS memory pressure, system disk capacity, Tart VM storage
  capacity, disk read/write throughput, uptime, and the time of the latest sample.
- **Samples every 60 seconds**, starting at launch. History holds up to 24 hours and
  resets when Tart Oven restarts — performance data isn't saved to disk.
- **Real CPU utilization** replaces the old load-average display.
- **No extra moving parts.** Metrics are collected in-process; there's no external
  service, agent, or dashboard asset to install.
- **Unavailable metrics say so** and don't hide the ones that did collect. The green,
  amber, and red colours are informational — they never trigger alerts or act on your
  VMs.
- Added `GET /api/performance`.

## 1.29 — 2026-08-21

### Prepare a base VM for Jamf

- **VM Management → Prepare base VM for Jamf** generates an enrollment profile from
  your saved Jamf Pro base URL and invitation ID and copies it to the guest's Desktop
  as `mdm_enroll.mobileconfig`.
- **The target picker shows running VMs only**, and the panel explains the intended
  workflow: prepare one running base image, stop it, and clone it — so each new VM
  gets the file without inheriting an already-enrolled identity.
- **Global SSH credentials** default to `admin` / `admin`. Per-VM credentials win
  where they're set.
- **The transfer is verified.** Tart Oven reads the file back from the guest and
  confirms it's byte-for-byte identical before reporting success, and each stage —
  configuration, VM selection, IP lookup, SSH authentication, upload, verification —
  reports its own clear failure.
- **No external `ssh` or `scp`.** The transfer uses a built-in SSH/SFTP client, so
  behaviour doesn't depend on what's installed on your Mac.
- **Duplicate requests are blocked** while a copy is already running.
- Added `POST /api/vm/mdm-profile`.

What Tart Oven does *not* do: install the profile inside the guest, stop the prepared
base VM, or create the clones. Those stay your call. The guest must be running,
reachable, and have Remote Login enabled before the copy.

### Secrets

- **Invitation IDs and SSH passwords are write-only** in API responses. The dashboard
  is told that a value exists, never what it is.
- **Blank means unchanged.** Submitting an empty password or invitation code keeps
  the saved one instead of erasing it.
- Values live in `~/.tart-oven/state.json`, written owner-only. They're meant for
  short-lived VM workflows, and they're kept out of logs and error messages.
- **Jamf base URLs are validated** as real HTTP or HTTPS addresses.

### VM boot reliability

- **Fixed: bridged VMs stopped seconds after starting** with
  `boot failure: no IP after 60s`, even though the VM had started correctly.

  The cause was on the host: when Tart ran underneath Tart Oven's background agent,
  its address-lookup subprocess came back empty, and Tart treated that as final even
  when asked to wait. Tart Oven read that as a full boot timeout and killed a
  perfectly healthy VM.

  Tart Oven now owns the boot deadline itself, reads the host's routing table
  directly, and matches the VM's MAC address against it. It keeps retrying until an
  address appears or your configured boot timeout expires.
- **VM names used for file lookups** are restricted to a single safe path component,
  with `..` explicitly rejected.
