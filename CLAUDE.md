# Agent instructions — adamomaton-platform

API aggregator + plugin host + temporal worker stack. Four Go modules; `dashboard/apiserver/` is the highest-conflict path in the umbrella.

## Before you edit

From the umbrella — and **always check conflicts** first if touching `dashboard/apiserver/`:

```bash
bin/adam status --conflicts
bin/adam claim platform/<task>
```

## Build / test

```bash
cd dashboard   && go build ./... && go test ./...
cd plugin-host && go build ./... && go test ./...
cd dispatch    && go build ./... && go test ./...
cd temporal    && go build ./... && go test ./...
```

From the umbrella, `go build ./...` covers all four via `go.work`.

## Gotchas

- **Each subdir is its own Go module.** Same rule as knowledge: per-module CI uses tagged `core` pin, local dev resolves via umbrella `go.work`.
- **Dashboard listens on `:9123`** — port 8080 is banned globally (memory: port-selection). Don't repick without updating Caddy + the frontend `VITE_EVO_API_BASE`.
- **Plugin-host gRPC on `:7375`**. Plugins are spawned as child processes via `core/subprocess` + `core/executor/cli`. Each plugin has its own README in `plugin-host-plugins/<name>/`.
- **CCSAVER mirror** (`dashboard/ccsaver-mirror/`) ships a 7-day slice from workstation → pi5 so the Pi dashboard can show rate-limit history without exposing the full CCSAVER DB. The rsync target is the host filesystem path.
- **DEPRECATED.md** in `dashboard/` flags the legacy Svelte dashboard (`:3141`, retired 2026-05-08). Don't resurrect it — the React frontend at `/delegator` is the replacement.
- **plugin_config schema** — when adding a new plugin row, also add an enable toggle on the Plugins admin page (`dashboard/ui/`).
- **Temporal worker registration** uses `core/workerregistry`. Don't add a worker without calling `Register(...)` on boot or it won't show on the `/nodes` page.

## Universal rules

See `../CLAUDE.md` in the umbrella.
