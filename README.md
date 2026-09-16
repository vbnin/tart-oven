# Tart Oven

Tart Oven is a local web console for managing [Tart](https://github.com/openai/tart) virtual machines on an Apple Silicon Mac. It can pull VM images, create runnable clones, start and stop guests, run commands, rotate VMs on a schedule, and show host and MDM status.

Tart Oven runs on macOS. Its guests can run macOS or Linux.

Current release: **1.53** · [Changelog](CHANGELOG.md)

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

Download `TartOven-1.53.pkg` from the [release page](https://github.com/vbnin/tart-oven/releases).

Open the package in Finder or install it from Terminal:

```sh
cd "$HOME/Downloads"
sudo installer -pkg "./TartOven-1.53.pkg" -target /
```

The package installs:

- `/Library/Application Support/Tart Oven/tart-oven`
- `/Library/LaunchAgents/com.tartoven.agent.plist`

It starts Tart Oven for the logged-in user and normally opens the dashboard. If the browser does not open, run:

```sh
open http://127.0.0.1:9000
```

### 2. Install Tart

If the dashboard says Tart is missing, click **Install Tart**. You can also open **Setup Wizard** and click **Install Tart CLI**.

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
3. In **VM Management**, enter a **Name prefix**.
4. Leave **How many to create** set to `1`.
5. Review **CPU cores**, **Memory (MB)**, and **Disk size (GB)**.
6. Keep **Random MAC** and **Random serial** selected.
7. Click **Create VMs**.
8. Wait for the clone task under **Logs → Activity** to finish.

Tart Oven appends a random eight-character suffix to the prefix. For example, `sequoia-` may become `sequoia-12AB34CD`.

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

### Images and VMs

Tart Oven keeps two kinds of entries separate:

- **OCI Images** are cached registry images used as clone sources.
- **Local VMs** are runnable, editable copies.

Pull an image once, then create local clones as needed. You can start a pull from **Dashboard → OCI Images**, or directly from **VM Management → Create / clone VMs** by picking **Pull OCI Image** alongside **Clone from template** and **Create from IPSW**.

### VM actions

The Dashboard provides **Run** and **Stop** next to each VM, plus a "⋯" menu with the rest:

- **Run** starts a stopped VM.
- **Stop** asks Tart to stop it with a short timeout and may force termination. Enable **Prioritize shutdown using SSH** in **Configuration → SSH & Commands** to instead ask the guest to shut down cleanly over SSH first (up to 30 seconds) before falling back to the fast stop — safer for guests that need time to flush state on power-off.
- **Get info** (⋯ menu) runs the configured status command inside the guest.
- **Install Agent** (⋯ menu) adds the Tart guest agent to a compatible guest that currently requires SSH fallback.
- **Start Screen Sharing** (⋯ menu) opens macOS Screen Sharing when the guest has an IP and Screen Sharing is enabled.
- **Edit Tags** (⋯ menu) opens the notes/tags editor for that VM.
- **Edit VM** (⋯ menu) jumps to **VM Management → Edit a VM** with that VM preselected.
- **Auto Enroll VM** (⋯ menu, experimental) — see [Auto-enrolling VMs at boot](#auto-enrolling-vms-at-boot-experimental) below.
- **Delete VM** (⋯ menu) permanently deletes that VM, after a confirmation prompt.

Official `ghcr.io/cirruslabs/macos-*-base` images include the Tart guest agent. Guest commands can then use `tart exec` without SSH credentials or guest networking.

For a custom guest without the agent, follow the **SSH setup guide** in **VM Management** or install the agent before creating more clones.

### Guest commands

Under **Dashboard → Guest Commands**:

1. Select a running local VM.
2. Enter a command, such as `sw_vers`.
3. Enter a sudo password only if the command requires one.
4. Click **Run**.

Commands execute with the privileges available inside the guest. Treat this panel like a terminal.

The **Jamf Commands** tab next to **SSH & Shell** offers one-click presets (`sudo jamf policy`, `sudo jamf recon`, `sudo jamf manage`) and a field to run `sudo jamf policy -event <name>` against a running VM, using the same guest-agent-or-SSH transport and sudo password field as SSH & Shell.

## Configuration

Open **Configuration** to change Tart Oven settings.

Important defaults include:

- **VM storage path (TART_HOME):** `/Users/Shared/Tart`
- **Shared dir (host_resources):** `/Users/Shared/Tart/Resources`
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

### Guest provisioning (macOS 27+)

**VM Management → Create from IPSW** has a "Guest provisioning" section that can seed a fresh guest's account (full name, username, password, auto-login, remote login) automatically, using Tart's `--provisioning-opts`. It only applies to that specific new VM's creation — the options are entered per create, not saved, and are consumed on the guest's very first boot, so cloning or pulling an OCI image always skips it, as does a Linux guest. This needs both the Tart Oven host and the new guest running macOS 27 or later, plus a tart build that supports `--provisioning-opts`; the section is greyed out with an explanatory hint until both are detected. Leave it off (the default) if you don't need it.

## Jamf and MDM

Tart Oven can generate a Jamf enrollment profile and copy it to a running base VM over SFTP.

A safe template workflow is:

1. Open **VM Management → Prepare base VM for Jamf**.
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

This script prepares a base VM so **Auto Enroll VM** and **Auto enroll at boot** actions work on every clone made from it afterward.

Prior to execute this script, complete the following requirements:

1. Create a clean base VM from an IPSW file or OCI source
2. Complete Setup Assistant if needed
3. Ensure FileVault is OFF
4. Enable autologin for the main user
5. Enable key-based SSH access or install Tart guest agent (see SSH setup guide below)
6. Deploy an enrollment profile on the VM's desktop

Keep Screen Sharing open to the VM while this script runs as it triggers a one-time TCC permission prompt to enable "sshd-keygen-wrapper" in Privacy settings. The script completes once a confirmation popup displays on the VM.

**VM Management → Prepare base VM for Jamf → Enable Auto-Enrollment Capabilities on Base VM** runs it:

1. Enable autologin on the base VM yourself first (**System Settings → Users & Groups → Login Options**). FileVault must be off for that option to be available.
2. Start the base VM and keep Screen Sharing open to it.
3. Select it under **Target Running VM** and click **Run script**.
4. Click **Allow** on the Automation prompt it triggers, and enable "sshd-keygen-wrapper" in the Accessibility settings it opens for you.
5. Watch for the confirmation dialog on the VM's own screen, or the banner in Tart Oven, once both grants are confirmed.

Use **Check compatibility** at any time to see a VM's current Autologin, Accessibility, and enrollment-profile status. A check reports Unknown rather than guessing when it can't complete — that is not the same as "not compatible."

These two permissions live on the base VM's disk, so every clone made from it afterward inherits them. It's a one-time step per base VM, not per clone.

### Enrolling automatically

With a primed base VM:

- **Auto enroll at first boot**, in **Clone from template**, flags each clone so its very next boot — scheduler-triggered or manual — runs the enrollment script on its own: push a fresh profile, click through Install and Enroll, then refresh the MDM column.
- **Auto Enroll VM**, in a VM's **⋯** menu on the dashboard, does the same on demand for one VM: it starts the VM first if it's stopped, or reuses it if it's already running.

Both handle the full first-boot Setup Assistant walkthrough automatically when it appears: `--random-serial` gives a clone a hardware identity macOS has never seen, so it re-runs the entire OOBE (network, Apple Account, FileVault) on its very first boot. The script recognizes and clicks through that before ever touching the enrollment profile. macOS 27 does not have this behavior, so a clone lands straight on the desktop and the script skips this step entirely.

This feature is experimental: it drives the same System Settings UI a person would, so a macOS point release that changes that UI's wording or layout can require an update.

## Automation

The HTTP API and Server-Sent Events stream use the dashboard's address.

Read the current state:

```sh
curl --fail-with-body http://127.0.0.1:9000/api/vms
```

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

Tart Oven is an administrative control plane. It has no user login, API token, or TLS.

Keep it bound to `127.0.0.1` unless you have separately secured access. Do not expose `0.0.0.0:9000` to an untrusted LAN or the internet.

Also note:

- Guest commands can execute arbitrary shell commands.
- Stored SSH passwords and Jamf invitation values are local but unencrypted.
- SSH and SFTP fallback do not verify guest host keys, so use them only on a trusted VM network.

## Troubleshooting

### The dashboard does not open

Run:

```sh
open http://127.0.0.1:9000
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

Tart Oven requires at least 25 GiB free on the filesystem containing **VM storage path (TART_HOME)**. Free space or choose another storage path, then retry.

### A VM starts but gets no IP

Tart Oven uses bridged networking.

Check that:

- The selected Wi-Fi or Ethernet interface is active.
- The LAN has a working DHCP server.
- **Configuration → Tart Settings → Network interface** matches the connected interface.
- **Boot timeout (s)** is long enough for the guest.

### Guest commands fail

For an official base image, wait for the guest to finish booting and retry **Get info**.

For a custom image:

- Install the Tart guest agent with **Install agent**, or
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
go build -o tart-oven .
./tart-oven -listen 127.0.0.1:9000
```

The source-run process stores state in `~/.tart-oven/state.json`.

Run both test suites after changing frontend or backend code:

```sh
go test ./... && node index_ui_test.js
```

## Support and license

Report bugs through the [GitHub issue tracker](https://github.com/vbnin/tart-oven/issues).

This repository does not currently include a license file. Ask the maintainer for applicable terms before redistributing the software.
