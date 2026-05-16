# dr-plugin-zotero

Zotero importer plugin for the deepresearch plugin-host. Replaces the
legacy in-process `app/zotero/` subsystem with a standalone subprocess
that talks gRPC over Unix sockets.

Two ingest modes:

- `web_api` — pyzotero-backed iterator over the user's Web API library.
- `sqlite_upload` — read a user-uploaded `zotero.sqlite` plus its
  `storage/` tree directly.

The plugin hands every row through `Host.IsKnown` for dedup, stages PDFs
via `Host.StagePath`, and persists row state via `Host.UpsertImportRow`
into `platform.zotero_imports` (the legacy table; column shape preserved).

See `platform/plugins/_sdk/` for the SDK and the host wire contract.
