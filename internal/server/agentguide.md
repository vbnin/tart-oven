# Tart Oven agent API

Use this API to get a fresh, disposable macOS VM, run commands in it, and throw it away.
Every VM is a clone of an approved template, runs on NAT networking, and is deleted
automatically when its lease runs out.

Send `Authorization: Bearer <agent token>` on every request. Errors look like
`{"error":{"code":"...","message":"..."}}`; branch on `code`.

## Workflow

1. `GET /api/agent/info`: see which templates you may clone, free VM slots and limits.
2. `POST /api/agent/vms` with `{"template":"<name>","label":"build","ttlMinutes":60}`.
   Returns `202` with the VM's `name`. Optional: `cpu`, `memory` (MB), `diskSize` (GB),
   `headless` (default true).
3. `GET /api/agent/vms/{name}/wait?timeout=300`: blocks until `ready` is true or the VM
   `failed`. `ready` means the VM is up and answers commands. If `timedOut` is true, call
   wait again.
4. `POST /api/agent/vms/{name}/exec` with `{"command":"sw_vers"}`. Returns
   `stdout`, `stderr`, `exitCode`, `timedOut`, `truncated`, `durationMs`. A command that
   exits non-zero is still HTTP 200; check `exitCode`.
5. `DELETE /api/agent/vms/{name}` when finished. Do this even on errors.

Extend a lease with `POST /api/agent/vms/{name}/extend` and `{"ttlMinutes":60}`; the new
expiry is now plus that many minutes. `GET /api/agent/vms` lists your VMs.

## Rules and limits

- The host runs at most **2 VMs at once**, shared with people and other agents. A create
  that finds no free slot fails with `409 capacity`; destroy something or retry later.
- Leases default to 60 minutes. When one expires the VM is stopped and **deleted**, with
  everything in it. Copy results out first.
- `exec` runs as the template's user, with passwordless `sudo` if the template has it.
  Options: `timeoutSec` (default 120, max 1800), `cwd`, `env` (object of strings). Output
  is capped at 1 MiB per stream; `truncated` tells you when it was cut. On timeout the
  command is abandoned (`timedOut: true`) but processes it started may keep running in
  the guest.
- `exec` needs `ready`; before that you get `409 not_ready`.
- Each command is a fresh shell, so state (cd, exports) does not carry over. Use `cwd`
  and `env`, or chain commands with `&&`.

## Moving files

Each VM has a shared folder, shown as `sharedFolder` in the VM's JSON:

- `host`: the folder on the Mac running Tart Oven.
- `guest`: the same folder inside the VM.

Write input there on one side and read it on the other. This is the way to get files in
and out; there are no upload or download endpoints. The folder is kept for 24 hours after
the VM is gone, then removed.

## Error codes

`agent_api_disabled` (403), `template_not_allowed` (403), `template_not_found` (404),
`template_busy` (409), `capacity` (409), `invalid_request` (400), `invalid_ttl` (400),
`not_found` (404; also for VMs that belong to another agent), `not_ready` (409),
`lease_expired` (409), `exec_failed` (502), `destroy_failed` (500).

A VM that fails to build or boot is deleted for you; `GET /api/agent/vms/{name}` then
shows `phase: "failed"` with an `error` for about an hour (`clone_failed`, `boot_failed`
or `capacity`). `phase: "stopped"` means the VM stopped unexpectedly; destroy it and
create a new one.

## Good practice

- Create one VM per task and destroy it when done. Do not keep VMs around "just in case".
- Never put secrets, tokens or personal data in the shared folder or in command lines you
  do not want logged: Tart Oven logs the first 100 characters of every command.
- The VM has outbound internet access through NAT. Do not give it credentials you would
  not hand to the code you run in it.
