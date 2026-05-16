# evo dashboard — DEPRECATED

This directory was an experiment in building a unified ops+UI panel for the
evo monorepo. It has been **demoted** and is **scheduled for harvest + removal**.

## What's actually deployed

Today on the Pi:

- `apiserver` (Go, `evo-api` container, port 9123) — **still serves live
  traffic**. The deepresearch frontend reaches it via Caddy's `/evo-api/*`
  matcher, which strips the prefix and injects a Bearer token.
- `ui` (Vite/React SPA) — **never deployed**. There is no Caddy route
  serving its `dist/`. The code exists but never reaches a browser.

## Going forward

The platform UI is `/thearray/git/deepresearch/platform/frontend/`. New pages,
new components, new design work all happen there.

Pieces from this directory get salvaged:

- **Memory page** — already ported to the deepresearch frontend; the Go
  handlers at `apiserver/memory_{files,db}.go` continue to back `/evo-api/api/v1/memory/*`
  until they migrate into `platform/backend/` (or get re-implemented in r2g).
- **Delegator dashboard** — `apiserver/delegator_endpoints.go` + the React
  pieces will be ported similarly.
- **System status / nodes / workers / skills / evo runs** — same fate;
  each will land in the deepresearch frontend with appropriate backend.

Anything that has no consumer once the migrations finish gets deleted.

## Rules while the harvest is in progress

1. **Do not extend this code.** New endpoints, new pages, new UI work go to
   the deepresearch frontend + platform-backend (or r2g, for performance-sensitive
   surfaces).
2. Bug fixes to currently-live endpoints (the Go handlers under `apiserver/`)
   are fine if they're keeping production working until migration.
3. Every Go and TSX/TS file in this tree carries a `DEPRECATED:` banner at
   the top. The banner is not a lint warning — it's a sign for humans + LLMs.

Last updated: 2026-05-15. PR commit: see `git log --grep="harvested"`.
