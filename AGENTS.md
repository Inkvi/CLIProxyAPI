# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
- This checkout is a personal fork, not the upstream project.
  - `origin`: https://github.com/Inkvi/CLIProxyAPI (push here)
  - `upstream`: https://github.com/router-for-me/CLIProxyAPI (never push here)
- Working branch: `soft-quota-threshold`, rebased on the latest upstream release tag.

## Fork Changes
- Soft quota threshold (`sdk/cliproxy/auth/soft_quota.go`, hooked into `MarkResult` in `conductor_cooldown.go`):
  - `routing.soft-quota-threshold` (config, 0 disables): pauses a Claude or Codex credential that can bill paid overage (Claude `Overage-Status: allowed`, Codex credits available or unlimited) once its 5h/7d or primary/secondary usage reaches the threshold, until the window resets.
  - `soft_quota_threshold` (auth JSON field): per-credential override that applies even without paid overage.
  - Reuses the existing `credential_quota` cooldown, so the selector and scheduler skip the credential. The pause is in memory only; a restart clears it, and the next response re-applies it.
- Keep fork changes small and isolated so rebasing onto new upstream releases stays easy.

## Syncing With Upstream
```bash
git fetch upstream --tags
git rebase <new-release-tag>          # e.g. v8.0.48
go test ./sdk/cliproxy/auth/ ./internal/config/
git push --force-with-lease origin soft-quota-threshold
```
Then deploy (below).

Some upstream tests already fail on a clean upstream checkout (timing-sensitive): `TestDevinApplyPatchRejectionCarriesLogDiagnostic`, `TestPionMediaRelayBridgesAudioAndDataChannel`, and occasionally `sdk/cliproxy` plugin staging and Codex websocket keepalive tests. Before blaming a change, run the same test on the release tag with `git stash`.

## Local Deployment (this machine)
The proxy that runs on this Mac is built from this checkout. There is no Homebrew install; do not `brew install cliproxyapi`, since it would fight over port 8317.

| What | Where |
|---|---|
| Binary | `bin/cliproxyapi` (gitignored), linked from `~/.local/bin/cliproxyapi` |
| Service | launchd agent `com.inkvi.cliproxyapi` (`~/Library/LaunchAgents/com.inkvi.cliproxyapi.plist`), starts at login, restarts on crash |
| Config | `~/.cli-proxy-api/config.yaml` (contains keys: never commit it or copy it into the repo) |
| Accounts | `~/.cli-proxy-api/*.json` |
| Logs | `~/Library/Logs/cliproxyapi.log` |
| Listens on | `127.0.0.1:8317` |

Deploy after changing code:
```bash
gofmt -w <changed files>
go test ./sdk/cliproxy/auth/ ./internal/config/   # plus packages you touched
go build -buildvcs=false -ldflags="-s -w \
  -X 'main.Version=$(git describe --tags --abbrev=0 | sed 's/^v//')-fork' \
  -X 'main.Commit=$(git rev-parse --short HEAD)' \
  -X 'main.BuildDate=$(date -u +%FT%TZ)' \
  -X 'main.DefaultConfigPath=$HOME/.cli-proxy-api/config.yaml'" \
  -o bin/cliproxyapi ./cmd/server
launchctl kickstart -k gui/$(id -u)/com.inkvi.cliproxyapi
```
- `DefaultConfigPath` lets `cliproxyapi -claude-login` / `-codex-login` work without `-config`.
- Commit before building so the embedded commit hash means something.

Verify:
```bash
bin/cliproxyapi --help 2>&1 | head -1                # Commit should equal: git rev-parse --short HEAD
launchctl print gui/$(id -u)/com.inkvi.cliproxyapi | grep -E '^\s*(state|program) ='
tail -n 20 ~/Library/Logs/cliproxyapi.log
```

No restart is needed for edits to `config.yaml` or account files; the proxy reloads them on its own.

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/home/` — CLIProxyAPIHome control plane integration (bootstrap, RESP communication, dispatch coordination)
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Payload configuration MUST be the final semantic barrier before sending requests in every executor, including streaming, WebSocket, continuation, retry/fallback, image, and token-count paths where applicable. Complete all built-in payload translation, normalization, injection, and cleanup first, then evaluate and apply user payload rules exactly once to the final business payload. No subsequent logic may overwrite configured values or restore filtered fields. Only necessary transport framing, serialization, signature calculation, and read-only validation may follow without changing business payload semantics. Add regression tests when introducing or changing request-building paths to enforce this ordering.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
- Endpoints under the `/v0/management` base URL are deprecated and no longer maintained. For any feature changes, do not modify endpoints under `/v0/management` unless necessary to fix compilation errors.
