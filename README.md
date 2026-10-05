# Tart Oven

Tart Oven is a local web console for managing [Tart](https://github.com/openai/tart) virtual machines on an Apple Silicon Mac. It can pull VM images, create runnable clones, start and stop guests, run commands, rotate VMs on a schedule, and show host and MDM status.

Tart Oven runs on macOS and manages macOS virtual machines.

Current release: **2.0** · [Changelog](CHANGELOG.md)

## Screenshots

<p align="center">
  <img src="assets/screenshots/Tart%20Oven%20Screenshot%201.png" alt="Tart Oven screenshot 1" width="900">
</p>
<p align="center">
  <img src="assets/screenshots/Tart%20Oven%20Screenshot%202.png" alt="Tart Oven screenshot 2" width="900">
</p>
<p align="center">
  <img src="assets/screenshots/Tart%20Oven%20Screenshot%203.png" alt="Tart Oven screenshot 3" width="900">
</p>
<p align="center">
  <img src="assets/screenshots/Tart%20Oven%20Screenshot%204.png" alt="Tart Oven screenshot 4" width="900">
</p>
<p align="center">
  <img src="assets/screenshots/Tart%20Oven%20Screenshot%205.png" alt="Tart Oven screenshot 5" width="900">
</p>

## Prerequisites

You need:

- An Apple Silicon Mac.
- macOS 13 Ventura or later.
- An administrator account for package installation.
- An active Wi-Fi or Ethernet connection with DHCP. Tart Oven uses bridged networking for its guests.
- At least 25 GiB free in the VM storage location. Allow more space for images and local clones.

Tart does not need to be installed first. Tart Oven can install it during setup.

Tart Oven limits the host to two running VMs at a time. Apple's macOS license also places conditions on virtualized macOS use; review the license that applies to your host and guest version.

## Quick start

### 1. Install Tart Oven

Download `TartOven-2.0.pkg` from the [release page](https://github.com/vbnin/tart-oven/releases).

Open the package in Finder or install it from Terminal:

```sh
cd "$HOME/Downloads"
sudo installer -pkg "./TartOven-2.0.pkg" -target /
```

The package installs:

- `/Library/Application Support/Tart Oven/tart-oven`
- `/Library/LaunchAgents/com.tartoven.agent.plist`
- `/usr/local/bin/tart-oven`, a link to the binary (see [Starting and stopping Tart Oven](#starting-and-stopping-tart-oven))
- The Tart guest agent installer, which Tart Oven copies into the shared folder of every guest

It starts Tart Oven for the logged-in user and normally opens the dashboard. If the browser does not open, run:

```sh
open http://127.0.0.1:9000
```

If you turned on HTTPS, use `https://` instead (see [Security](#security)).

### 2. Install Tart

If the dashboard says Tart is missing, click **Install Tart**. You can also open **Configuration → Setup Wizard** and click **Install Tart CLI**.

Wait for the installation task to finish before continuing.

### 3. Pull a base image

1. On **Dashboard**, click **Pull OCI Image**.
2. Choose a curated image, such as **macOS 15 (Sequoia)**.
3. Click **Pull Image**.
4. Open **Logs** and wait for the pull task under **Activity** to finish.

A cached OCI image is a source for cloning. It is not the local VM you will run.

### 4. Create a local VM

1. Return to **Dashboard**.
2. Under **OCI Images**, click **Clone** beside the image you pulled.
3. In **VM Management**, enter a **Name template**, such as `sequoia-$AUTONUM`.
4. Leave **How many to create** set to `1`.
5. Review **CPU cores**, **Memory (MB)**, and **Disk size (GB)**.
6. Keep **Random MAC** and **Random serial** selected.
7. Click **Create VMs**.
8. Wait for the clone task under **Logs → Activity** to finish.

The template is the VM's name, and may use two variables (click the **?** beside the field):

- `$RAND8` becomes 8 random capital characters (0-9, A-F), such as `12AB34CD`. A blank template is just `$RAND8`.
- `$AUTONUM` becomes the next free number: `sequoia-$AUTONUM` gives `sequoia-1`, `sequoia-2`, and so on. Numbers already in use are skipped.

If a VM with the same name already exists, `-1`, `-2`, and so on is added (`lab` becomes `lab-1`). Names can't contain `/`, `:` or `\`, or start with `-` or `.`.

### 5. Run it

1. Open **Dashboard**.
2. Find the new VM under **Local VMs** and click **Run**.
3. Wait until its state is **running** and an IP address appears.
4. Click **Get info**.

A successful result shows the guest hostname, serial number, and macOS version. That is your first working Tart Oven VM.

For the curated Cirrus Labs macOS images, you can also click **Screen**. Their default login is:

- Username: `admin`
- Password: `admin`

## Basic usage

### The dashboard at a glance

The tabs along the top are:

- **Dashboard:** your **Local VMs** and the cached **OCI Images**.
- **Performance:** host CPU, memory, memory pressure and disk use, with up to 24 hours of one-minute samples.
- **VM Management:** create, clone and pull VMs, prepare a base VM for Jamf, and the bundled Tart guest agent.
- **Configuration:** scheduler, Tart, SSH and server settings, and the Setup Wizard.
- **Logs:** Tart logs, **Activity** (pulls and clones in progress) and run history.
- **Helper Guide:** this document.

On the Dashboard:

- **+ Create VM** jumps to **Create / clone VMs**. **Scheduler** and **Refresh** sit beside it, and **Show All / Show Running** filters the list. **Refresh** also re-checks the guest agent and SSH for running VMs.
- Click a column name, or its arrows, to sort the **Local VMs** and **OCI Images** tables. The choice is remembered.
- The **Access** column shows whether commands can reach the guest: **Agent** (the Tart guest agent answers) or **SSH** (the fallback). When the agent works, only it is shown. A stopped VM keeps its last known status, faded.
- The **ⓘ** button opens **VM details**: state, IP, last start and stop, uptime, hardware, Access, MDM enrollment, the last **Get info** output, tags and notes. It updates live.
- Click a **?** beside a setting for its help text. Click a section title to collapse it; Tart Oven remembers which are open. The switch in the header toggles dark and light mode.
- Leaving **Configuration** with unsaved changes asks whether to save, discard or keep editing.

The **Setup Wizard** (Configuration) opens by itself on a first run with no VMs, and you can relaunch it any time. It has five steps:

1. **Environment** checks that the Mac has an Apple silicon chip, and that Tart is installed and up to date. It can install or update Tart for you.
2. **Storage & Server** shows the VM storage path, which you can change, and the free space there. Tart Oven recommends 40 GB or more, and warns if there is less, but you can continue. It also shows the server address.
3. **First VM** offers two routes. **Pull an OCI image** is the quickest: it downloads a prepared macOS image you can clone. **Build a fresh VM from an IPSW** starts from Apple's restore image for a clean install, in the macOS version you choose. Pick one, then pull the image or choose a version from the list. You can also skip this step.
4. **Purpose** sets starting defaults. **Testing / Troubleshooting** keeps the scheduler off and runs VMs with a full display and audio. **Demo / Data Generation** turns the scheduler on and runs scheduled VMs headless with audio off. Change any of it later in Configuration.
5. **Review** summarises your choices.

### Updates

Tart Oven checks GitHub for new Tart and Tart Oven releases at startup and every 24 hours. A banner in the top right links to **Update Tart** or to the Tart Oven release page. Closing a banner hides it until a newer version appears or Tart Oven restarts. Turn each check off with **Check for Tart updates** (Tart Settings) and **Check for Tart Oven updates** (Server Settings).

### Images and VMs

Tart Oven keeps two kinds of entries separate:

- **OCI Images** are cached registry images used as clone sources.
- **Local VMs** are runnable, editable copies.

Pull an image once, then create local clones as needed. You can start a pull from **Dashboard → OCI Images**, or directly from **VM Management → Create / clone VMs** by picking **Pull OCI Image** alongside **Clone from template** and **Create from IPSW**.

### Starting and stopping Tart Oven

The package installs a `tart-oven` command in `/usr/local/bin`. From Terminal:

```sh
tart-oven open      # start if needed, then open the dashboard
tart-oven start     # start the server in the background
tart-oven stop      # stop it (same as Stop server in the UI)
tart-oven restart
tart-oven status    # running or not, version and URL
tart-oven token generate   # create or rotate the dashboard access token
tart-oven token revoke     # remove the token
```

`tart-oven` with no command runs the server in the foreground, which is what the LaunchAgent does. If **Launch at login** is off, `start` runs the server without launchd until you stop it or log out.

### VM actions

The Dashboard provides **Run** and **Stop** next to each VM, plus a "⋯" menu with the rest:

- **Run** starts a stopped VM.
- **Run with arguments** (⋯ menu) opens a window to choose `tart run` options for one run: display (headless, Screen Sharing, or Virtualization VNC, which works before login and in recovery), network (shared NAT, host only, Softnet) and options such as no audio or boot into recovery. The choice replaces **Custom run arguments** for that run, and **Restart VM** keeps it. It opens pre-filled with what a normal run would use.
- **Restart VM** (⋯ menu) restarts a running VM, keeping the options chosen in **Run with arguments**.
- **Stop** asks Tart to stop it with a short timeout and may force termination. Enable **Prioritize clean shutdown** in **Configuration → SSH & Commands** to instead ask the guest to shut down cleanly (via the guest agent or SSH) first (up to 30 seconds) before falling back to the fast stop — safer for guests that need time to flush state on power-off.
- **Get info** (⋯ menu) runs the configured status command inside the guest.
- **Install Agent** (⋯ menu) installs the bundled Tart guest agent package into a running guest over SSH, so it no longer needs SSH fallback for commands.
- **Start Screen Sharing** (⋯ menu) opens macOS Screen Sharing when the guest has an IP and Screen Sharing is enabled.
- **Edit VM** (⋯ menu) opens a window with the VM's hardware (CPU, memory, disk, display), **Rename to** (accepts the same `$RAND8` and `$AUTONUM` variables as a name template), new random MAC and serial, hostname, SSH credentials, enrollment, and tags and notes. While the VM runs, only tags, notes, SSH credentials and auto-enroll can change; stop it for the rest.
- **Hostname** (Edit VM, or Create / clone VMs) sets the guest's computer name, local hostname and hostname with `scutil`, either to a custom name or **Same as VM name**. It's applied while the VM is running, or at its next boot, and needs the guest agent or SSH plus the guest's sudo password. Characters other than letters, digits and hyphens become hyphens in the network names.
- **Auto enroll at next boot** (Edit VM → Enrollment, experimental) — see [Auto-enrolling VMs at boot](#auto-enrolling-vms-at-boot-experimental) below.
- **Delete VM** (⋯ menu) permanently deletes that VM, after a confirmation prompt.

Tart Oven only manages macOS guests; Linux VMs are not supported.

Official `ghcr.io/cirruslabs/macos-*-base` images include the Tart guest agent. Guest commands can then use `tart exec` without SSH credentials or guest networking.

For a custom guest without the agent, use **⋯ → Install Agent** (requires SSH), or manually install it from the VM's shared folder (Finder → My Shared Files → host_resources → tart-guest-agent → double-click the .pkg; **VM Management → Tart guest agent** also has a **Download PKG** button). Install it once on a base VM before cloning.

### Guest commands

Each VM on the Dashboard has a **`>_`** button next to its **Stop** button. Click it to open that VM's terminal window.

1. Enter a command, such as `sw_vers`.
2. Enter a sudo password only if the command requires one.
3. Click **Run** or press Enter.

Commands execute with the privileges available inside the guest. The terminal window shows live guest agent and SSH status at the top, and each VM remembers its own console output, command draft, and sudo password across open/close cycles.

Shortcut buttons below the command field let you run common Jamf commands in one click: `jamf manage`, `jamf recon`, `jamf policy`, and `jamf checkJSSConnection`. They appear when **Configuration → Server Settings → Display Jamf-related features** is on.

## Configuration

Open **Configuration** to change Tart Oven settings. Click **Save configuration** at the bottom to apply them.

Important defaults include:

- **VM storage path:** `/Users/Shared/Tart` (Tart's `TART_HOME`)
- **VM shared directory:** `/Users/Shared/Tart/Resources` (appears in each guest as `host_resources`)
- **Tart binary path:** `/Applications/tart.app/Contents/MacOS/tart`
- **Listen:** `127.0.0.1:9000`
- **Scheduler:** paused
- **Maximum concurrent VMs:** `1`, configurable up to `2`
- **OCI images excluded from scheduling:** enabled

Restart Tart Oven after changing **Listen**.

### Scheduler

The scheduler can rotate stopped local VMs in sequential or random order.

You can configure:

- How often it acts.
- How long each VM runs.
- Maximum concurrent VMs.
- Daily active hours.
- VMs that should never be selected.
- Headless mode and audio.

The scheduler is off until you start it. When running, it stops VMs whose configured run window expires. Outside configured daily hours, it can stop all running VMs, including VMs started manually.

Keep **Exclude OCI images from scheduler** enabled so registry cache entries remain clone sources.

### Creating a VM from an IPSW

**VM Management → Create from IPSW** builds a new macOS VM from a restore image. Pick a version from the **macOS version** list, which Tart Oven loads from [AppleDB](https://github.com/littlebyteorg/appledb) and refreshes daily, or click **Browse…** to choose a local `.ipsw` in a Finder window (only available in a browser on the Mac running Tart Oven). You can also type or paste a path or an `http(s)://` URL into **IPSW path or URL**; the old `latest` shortcut is gone. Tick **Include betas** to list pre-release builds. Only builds Apple still signs for virtual Macs are listed; your Mac must be able to run the macOS version you pick.

### Guest provisioning (macOS 27+)

**VM Management → Create from IPSW** has a "Guest provisioning" section that can seed a fresh guest's account (full name, username, password, auto-login, remote login) automatically, using Tart's `--provisioning-opts`. It only applies to that specific new VM's creation — the options are entered per create, not saved, and are consumed on the guest's very first boot, so cloning or pulling an OCI image always skips it. This needs both the Tart Oven host and the new guest running macOS 27 or later, plus a tart build that supports `--provisioning-opts`; the section is greyed out with an explanatory hint until both are detected. Leave it off (the default) if you don't need it.

## Jamf and MDM

Tart Oven can generate a Jamf enrollment profile and copy it to a running base VM over SFTP.

A safe template workflow is:

1. Turn on **Configuration → Server Settings → Display Jamf-related features** (off by default), then open **VM Management → Prepare VM for Jamf**.
2. Click **Add Jamf Server** and enter the server name, Jamf Pro base URL, and invitation ID.
3. Start the base VM.
4. Select the running VM and Jamf server profile.
5. Click **Copy profile to Desktop** — it uses that VM's own SSH username/password if set, otherwise the defaults from Configuration.
6. Confirm `~/Desktop/mdm_enroll.mobileconfig` exists in the guest.
7. Stop the base VM without enrolling it.
8. Clone it with **Random MAC** and **Random serial** enabled.
9. Start and enroll each clone separately.

The **MDM** column is updated when Tart Oven probes a guest:

- Grey means it has not been checked.
- Red means no enrollment was reported.
- Green shows an enrolled server.

Jamf invitation values and guest SSH passwords are stored locally in `~/.tart-oven/state.json`. The file is owner-only, but the values are not encrypted.

## Auto-enrolling VMs at boot (experimental)

Copying the profile by hand still works, but Tart Oven can also drive System Settings itself — clicking through Install and Enroll over SSH — so a batch of freshly-cloned VMs can enroll themselves with no further intervention.

### Priming a base VM

This script prepares a base VM to support the Auto Enrollment VM feature on new VMs cloned from it.

Prior to execute this script, complete the following requirements:

1. Create a clean base VM from an IPSW file or OCI source
2. Complete Setup Assistant if needed
3. Ensure FileVault is OFF
4. Enable autologin for the main user
5. Install Tart guest agent (or enable key-based SSH access)

Keep Screen Sharing open to the VM while this script runs as it triggers a one-time TCC permission prompt to enable "sshd-keygen-wrapper" in Privacy settings. The script completes once a confirmation popup displays on the VM.

**VM Management → Prepare VM for Jamf → Enable Auto-Enrollment Capabilities on Base VM** runs it:

1. Enable autologin on the base VM yourself first (**System Settings → Users & Groups → Login Options**). FileVault must be off for that option to be available.
2. Start the base VM and keep Screen Sharing open to it.
3. Select it under **Target Running VM** and click **Run script**.
4. Click **Allow** on the Automation prompt it triggers, and enable "sshd-keygen-wrapper" in the Accessibility settings it opens for you.
5. Watch for the confirmation dialog on the VM's own screen, or the banner in Tart Oven, once both grants are confirmed.

Use **Check compatibility** at any time to see a VM's current Autologin and Accessibility status. A check reports Unknown rather than guessing when it can't complete — that is not the same as "not compatible."

These two permissions live on the base VM's disk, so every clone made from it afterward inherits them. It's a one-time step per base VM, not per clone.

### Enrolling automatically

With a primed base VM:

- **Auto enroll at first boot**, in **Clone from template**, is a list of your Jamf server profiles. Choose one and each clone is flagged so its very next boot — scheduler-triggered or manual — runs the enrollment script on its own: push that server's profile, click through Install and Enroll, then refresh the MDM column. Leave it on **None** to skip.
- **Auto enroll at next boot**, in **Edit VM → Enrollment**, does the same once, the next time the VM starts. It clears itself after that attempt, whether it succeeds or fails.

Because the chosen profile is pushed to the clone, the base VM doesn't need one: keep it un-enrolled and choose a different server for each batch of clones.

Both handle the full first-boot Setup Assistant walkthrough automatically when it appears: `--random-serial` gives a clone a hardware identity macOS has never seen, so it re-runs the entire OOBE (network, Apple Account, FileVault) on its very first boot. The script recognizes and clicks through that before ever touching the enrollment profile. macOS 27 does not have this behavior, so a clone lands straight on the desktop and the script skips this step entirely.

This feature is experimental: it drives the same System Settings UI a person would, so a macOS point release that changes that UI's wording or layout can require an update.

## Automation

The HTTP API and Server-Sent Events stream use the dashboard's address.

Read the current state:

```sh
curl --fail-with-body http://127.0.0.1:9000/api/vms
```

With an access token set (see [Security](#security)), add `-H "Authorization: Bearer <token>"` to every request. Use `https://` and `-k` (or `--cacert`) when HTTPS is on with a self-signed certificate.

Start a VM:

```sh
curl --fail-with-body \
  -X POST http://127.0.0.1:9000/api/run \
  -H "Content-Type: application/json" \
  -d '{"name":"<vm-name>"}'
```

The response only confirms that Tart Oven accepted the request. Watch `/api/vms` or `/events` for the actual result.

Run a guest command:

```sh
curl --fail-with-body \
  -X POST http://127.0.0.1:9000/api/exec \
  -H "Content-Type: application/json" \
  -d '{"name":"<vm-name>","command":"sw_vers"}'
```

Stream updates:

```sh
curl -N http://127.0.0.1:9000/events
```

## Security

Tart Oven is an administrative control plane: whoever can reach it can run commands in your guests. By default it listens on `127.0.0.1` and needs no login. If you open it to a network, turn on both protections below. On the internet, prefer a VPN or a reverse proxy in front of it.

### Access token

In **Configuration → Server Settings**, choose **Generate token**. From then on the dashboard and API require it. The token is shown once; Tart Oven keeps only a hash, so a lost token can't be looked up, only replaced.

- Browsers show a sign-in prompt and keep an HttpOnly session for 7 days. Five wrong tries lock that address out for a minute.
- Scripts send `Authorization: Bearer <token>`. Only the page shell, the icon, and the `/api/health` and `/api/auth/*` sign-in routes work without it.
- **Rotate token** in the dashboard, or `tart-oven token generate` in Terminal, replaces the token and signs everyone out. `tart-oven token revoke` removes it. The Terminal commands work when you have lost the token, because they act on the files in `~/.tart-oven`, and the running server notices the change at once.
- `tart-oven stop` and `restart` keep working with a token set.

### HTTPS

In **Configuration → Server Settings**, turn on **Serve over HTTPS**, save, and restart Tart Oven (**Restart server**, or `tart-oven restart`). Tart Oven reminds you after you save.

- To use your own certificate, enter the paths of a PEM certificate and key. Tart Oven checks that they load when you save, and refuses to start if they stop loading (it never falls back to plain HTTP).
- With both paths empty, or after **Generate self-signed certificate**, Tart Oven creates a certificate in `~/.tart-oven/tls`, valid for a year and renewed automatically within 30 days of expiry. Browsers warn about a self-signed certificate; compare the SHA-256 fingerprint shown in Server Settings before accepting it.
- Open the dashboard at `https://` after the restart. The `tart-oven` command detects HTTPS by itself.
- If a bad certificate stops the server, set `"tlsEnabled": false` in `state.json` (while the server is stopped) and start it again.

Also note:

- The access token and HTTPS protect the dashboard only; guests and the host are not otherwise isolated.
- Guest commands can execute arbitrary shell commands.
- Stored SSH passwords and Jamf invitation values are local but unencrypted.
- SSH and SFTP fallback do not verify guest host keys, so use them only on a trusted VM network.

## Troubleshooting

### The dashboard does not open

Run:

```sh
tart-oven status
tart-oven open
```

Check the application log:

```sh
tail -n 100 "$HOME/Library/Logs/tart-oven.log"
```

Package-launch output is written to:

```text
/Users/Shared/tart-oven.out.log
/Users/Shared/tart-oven.err.log
```

### Tart is missing

Use **Install Tart** in the dashboard. The current upstream Homebrew command is:

```sh
brew install openai/tools/tart
```

The default Tart Oven binary path is `/Applications/tart.app/Contents/MacOS/tart`. If your installation differs, update **Configuration → Tart Settings → Tart binary path**.

### A pull reports insufficient disk space

Tart Oven requires at least 25 GiB free on the filesystem containing **VM storage path**. Free space or choose another storage path, then retry.

### A VM starts but gets no IP

Tart Oven uses bridged networking by default.

Check that:

- The selected Wi-Fi or Ethernet interface is active.
- The LAN has a working DHCP server.
- **Configuration → Tart Settings → Network interface** matches the connected interface.
- **Boot timeout (s)** is long enough for the guest.

### A VM gets an IP but has no internet access

The guest picks up a DHCP lease and looks configured (check inside the
guest: an active interface with a valid IP/DNS), but it can't reach its own
gateway — `ping`/`curl` to the gateway or anything beyond it fails with
"Host is down" or "No route to host". This is typically an enterprise
Wi-Fi network enforcing client or MAC isolation: the access point accepts
the host Mac's own MAC address but silently drops traffic from the guest's
second (bridged) MAC, since to the network it looks like a second device.
Home networks and most wired switches don't do this, which is why the same
VM can work fine on one network and fail on another.

There's no Mac-side fix for this — it's enforced by the network. Switch
**Configuration → Tart Settings → Network interface** to **Shared (NAT)**:
guest traffic then routes out through the host's own already-connected
interface instead of appearing as a separate device on the wire. The
guest loses its own LAN-reachable IP in this mode (no direct SSH/MDM
reachability from other devices), but gets full outbound internet access.

### Guest commands fail

For an official base image, wait for the guest to finish booting and retry **Get info**.

For a custom image:

- Install the bundled Tart guest agent package with **⋯ → Install Agent** (requires SSH), or manually from the VM's shared folder (see **VM Management → Tart guest agent**), or
- Enable **Allow SSH fallback for guest commands** and follow the **SSH setup guide**.

### Screen Sharing fails

Make sure the VM has an IP and Screen Sharing is enabled inside the guest under **System Settings → General → Sharing**.

The curated Cirrus Labs macOS images use `admin` / `admin`.

### A start is deferred for critical memory pressure

Tart Oven blocks new starts while the latest macOS memory-pressure sample is critical. Running VMs are left alone. Reduce host load or stop an unused VM, then retry after pressure falls.

The HTTP request may still return `{"ok":true}` because startup runs in the background. Check the VM's error text in the Dashboard.

### A headless host cannot start a VM

On macOS 15 or later, Tart may require the host user's `login.keychain` to exist and be unlocked. See the [Tart headless-host guidance](https://tart.run/faq/#headless-machines).

## Build from source

Building requires Go 1.24.3 or later.

```sh
git clone https://github.com/vbnin/tart-oven.git
cd tart-oven
go build -o tart-oven ./cmd/tart-oven
./tart-oven -listen 127.0.0.1:9000
```

The source-run process stores state in `~/.tart-oven/state.json`.

To build the installer package for distribution, including the bundled guest agent:

```sh
# Build the guest agent PKG (unsigned for development)
SIGN_PKG=false ./packaging/build-agent-pkg.sh

# Build the Tart Oven PKG (includes the guest agent)
./packaging/build-pkg.sh
```

Run both test suites after changing frontend or backend code:

```sh
go test ./... && node web/index_ui_test.js
```

## Support and license

Report bugs through the [GitHub issue tracker](https://github.com/vbnin/tart-oven/issues).

Tart Oven is released under the [MIT License](LICENSE).

Tart and the Tart guest agent are separate projects under their own licenses (FSL-1.1-ALv2). The guest agent installer bundled with Tart Oven keeps its upstream license.
