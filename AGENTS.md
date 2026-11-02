# Tart Oven — Development & Testing Workflow Rules

## 1. Subagent Task Delegation
- **Task-by-Task Subagent Spawning**: Spawn specialized subagents on a per-task basis for deep research, code reviews, and parallel verification of changes.
- **Agent Tracking**: Track each subagent's progress, responsibilities, and outputs using subagent management tools.

## 2. Implementation & Test Suite Execution
- Maintain strict code quality and test coverage.
- After every frontend or backend modification, execute both test suites:
  ```bash
  go build -o tart-oven ./cmd/tart-oven && go test ./... && node web/index_ui_test.js
  ```

## 3. Package Build & Installation
- Build the signed macOS installer package:
  ```bash
  SIGN_PKG=false OUT_DIR=. ./packaging/build-pkg.sh
  ```
- Deploy and upgrade the local installation:
  ```bash
  sudo installer -pkg "./TartOven-1.50.pkg" -target /
  ```

## 4. End-to-End Verification on Live Server
- Inspect and verify the running daemon at `http://127.0.0.1:9000/`.
- Validate live DOM rendering, SSE event streams (`/events`), and guest command execution.
- Continuously iterate through this build-install-verify loop until all features and bugfixes are verified end-to-end.
