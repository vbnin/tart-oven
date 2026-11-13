# Tart Oven: development and testing workflow

## 1. Build and test after every change

```bash
go build -o tart-oven ./cmd/tart-oven && go test ./...
```

`go test ./...` is the gate. It also runs the JavaScript UI suite (`web/index_ui_test.js`, through `web/node_test.go`), which is skipped when `node` isn't installed. `web/index_test.go` pins UI element IDs and markup, so update it when you remove UI. New behaviour ships with its test in the same commit, and bug fixes come with a regression test.

## 2. Test against a sandbox, never a real install

Run a separate instance with its own state file, storage path, shared folder and port, with update checks off:

```bash
./tart-oven -state /tmp/tart-oven-sandbox/state.json
```

Never start a second instance on a real `TART_HOME`. Check UI changes in a real browser, in both light and dark themes.

## 3. Package and install

Build an unsigned installer package for local testing:

```bash
SIGN_PKG=false OUT_DIR=. ./packaging/build-pkg.sh
sudo installer -pkg "./TartOven-<version>.pkg" -target /
```

`<version>` is `const Version` in `tartoven.go`. To update an installed binary without a package, remove the old file before copying the new one, then stop and start the service (`tart-oven stop && tart-oven start`). Copying over a running signed binary gets it killed by macOS.

## 4. Verify end to end

Check the running server at `http://127.0.0.1:9000/`: the rendered page, the SSE stream at `/events`, and guest command execution. Repeat build, install and verify until every change is confirmed.
