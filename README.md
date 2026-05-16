# adamaton-platform

Always-on backend tier. Four Go modules: the dashboard API aggregator, the plugin-host that loads 14 search-source plugins, the dispatch worker, and the Temporal worker stack (gitea webhook + workflow starters + health checks).

## Layout

| Dir | Purpose |
|---|---|
| `dashboard/` | API aggregator. Listens on **:9123**. Owns `/api/v1/*` for the frontend |
| `dashboard/apiserver/` | Route handlers + HTTP server |
| `dashboard/ui/` | Static admin UI (separate from the `frontend/` SPA) |
| `dashboard/ccsaver-mirror/` | Pi-side mirror of the workstation CCSAVER rate-limit DB |
| `dashboard/reindex-arxiv-latex/` | Maintenance helper for arxiv latex reindexing |
| `plugin-host/` | gRPC plugin host (proto in `proto/`, generated stubs in `gen/`). Listens on **:7375** |
| `plugin-host-plugins/` | 14 search plugins (arxiv, github, hf_papers, jina, linkup, openalex, openreview, searxng, semantic_scholar, stackexchange, tavily, wiki_crawler, wikipedia) + zotero connector + shared `_sdk/` + `_tools/` |
| `dispatch/` | Temporal worker that dispatches plugin-host calls into long-running activities |
| `temporal/` | Temporal worker stack: `gitea-review`, `gitea-webhook`, `start-workflow`, `worker-health` |

## Build

Each module independently:

```bash
cd dashboard && go build ./...
cd plugin-host && go build ./...
cd dispatch && go build ./...
cd temporal && go build ./...
```

The plugin-host plugins are independent processes (Go or Python); each has its own build per directory.

From the umbrella, `go build ./...` covers all four Go modules via `go.work`.

## Test

```bash
cd <module> && go test ./...
```

## Dev DSN / endpoints

- Postgres: `postgres://evo:evo@localhost:5432/evo`
- Temporal: `localhost:7233`
- Dashboard HTTP: **:9123** (the `/api/v1/*` surface for the frontend)
- Plugin-host gRPC: **:7375**
- Plugin-host search HTTP (legacy passthrough): `/platform/search/query?source=...`

Schemas owned: `evo.plugin_config` (plugin enable/disable + per-plugin secrets reference).

## Where this fits

The dashboard is the **API aggregator** — by design it imports `adamaton-delegator/delegator` in-process and calls `adamaton-knowledge/skills-rae` and `adamaton-deepresearch/nano-research` over HTTP. The frontend talks only to dashboard. plugin-host is called by deepresearch (for search-stage queries) and by the dashboard (for the `/library` import flows). Depends on `adamaton-core`. See `docs/ARCHITECTURE.md` in the umbrella.
