"""Zotero importer plugin -- gRPC façade around the legacy sync logic.

The plugin is decorated with the SDK's @plugin / @importer markers; the
SDK's serve.py picks them up at startup, hands us a HostClient on each
Sync call, and turns whatever we ``emit.item`` / return into the wire's
SyncEvent stream.

The flow per item mirrors the legacy ``_process_item``:

  1. Dedup via ``host.is_known``.
  2. If the item has a local PDF, stage it via ``host.stage_path`` and
     copy the bytes to the returned path so the ingest worker can read
     it under the shared dr_uploads volume.
  3. Upsert a row into ``platform.zotero_imports`` via
     ``host.upsert_import_row`` so the same UI continues to work.
  4. Emit a ``PluginItem`` so the host's generic plugin_runs accounting
     (and, when ingest moves into the plugin world, the plugin_items
     table) sees the item.
"""

from __future__ import annotations

import logging
import shutil
import uuid
from typing import Any

from dr_plugin_sdk import importer, plugin
from dr_plugin_sdk.host_client import HostClient
from dr_plugin_sdk.types import (
    Collection,
    PluginItem,
    RunSummary,
    SyncArgs,
)

from .client import ZoteroClient
from .dedup import find_existing
from .normalize import ZoteroItemNormalized
from .sync import iter_items, zotero_user_id

logger = logging.getLogger(__name__)


@plugin(manifest="../../plugin.json")
class Plugin:
    """SDK-decorated plugin class. Methods are async, no I/O in __init__."""

    # ------------------------------------------------------------------
    # ListCollections
    # ------------------------------------------------------------------

    @importer.list_collections
    async def list(self, options: dict[str, Any] | None = None) -> list[Collection]:
        """Return the user's Zotero collections.

        For ``sqlite_upload`` mode we can't enumerate collections without
        an extra join on the sqlite tree -- return a single synthetic
        bucket so the UI has something to show. Web API mode lists the
        real top-level collections.
        """

        cfg = await self._config()
        source = cfg.get("source")
        if source == "web_api":
            return await _list_web_api_collections(cfg)
        # sqlite_upload or unconfigured -- one logical "everything" bucket.
        return [Collection(id="all", name="All items", item_count=0)]

    # ------------------------------------------------------------------
    # Sync
    # ------------------------------------------------------------------

    @importer.sync
    async def sync(
        self,
        args: SyncArgs,
        *,
        emit: Any,
        host: HostClient | None,
    ) -> RunSummary:
        """Walk the library, dedup, stage PDFs, persist rows, emit items."""

        if host is None:
            # DR_HOST_SOCK was unset -- we're being invoked outside the
            # plugin-host (likely in a unit test). Fail loudly: there's
            # nothing useful we can do without a host channel.
            emit.error("no_host", "no DR_HOST_SOCK; cannot reach plugin-host", fatal=True)
            return RunSummary()

        cfg = await _config_from_host(host)
        source = cfg.get("source")
        creds = _creds_from_config(cfg)
        if not source:
            emit.error("not_configured", "plugin has no saved config (source missing)", fatal=True)
            return RunSummary()

        only_with_pdf = bool(args.options.get("only_with_pdf", False))
        # ``since`` in the args overrides the config; web_api only.
        since_arg = args.options.get("since")
        since: int | None
        try:
            since = int(since_arg) if since_arg is not None else None
        except (TypeError, ValueError):
            since = None

        seen = 0
        new_items = 0
        deduped = 0
        errored = 0
        skipped = 0

        try:
            async for raw, normalized in iter_items(
                source, creds, only_with_pdf=False, since=since
            ):
                seen += 1

                if only_with_pdf and not normalized.has_pdf:
                    skipped += 1
                    emit.progress("skipped:no-pdf", seen=seen)
                    continue

                try:
                    outcome = await _process_item(
                        host=host,
                        run_id=args.run_id,
                        creds=creds,
                        normalized=normalized,
                        raw=raw,
                    )
                except Exception as exc:  # noqa: BLE001 - one bad item shouldn't fail the run
                    logger.exception(
                        "zotero plugin: item %s failed", normalized.zotero_key
                    )
                    errored += 1
                    emit.error(
                        "item_failed",
                        f"{normalized.zotero_key}: {type(exc).__name__}: {exc}",
                        fatal=False,
                    )
                    continue

                if outcome == "duplicate":
                    deduped += 1
                    emit.progress("deduped", seen=seen)
                else:
                    new_items += 1
                    emit.item(
                        PluginItem(
                            plugin_id="zotero",
                            external_id=normalized.zotero_key,
                            external_url=_zotero_web_url(creds, normalized),
                            title=normalized.title or "",
                            markdown_body="",  # ingest pipeline turns the PDF into chunks
                            metadata={
                                "canonical_id": normalized.canonical_id,
                                "canonical_kind": normalized.canonical_kind,
                                "doi": normalized.doi or "",
                                "arxiv_id": normalized.arxiv_id or "",
                                "isbn": normalized.isbn or "",
                                "has_pdf": normalized.has_pdf,
                            },
                        )
                    )

        except Exception as exc:  # noqa: BLE001 - top-level fail surfaces as a fatal SyncError
            logger.exception("zotero plugin: top-level failure")
            emit.error("top_level", f"{type(exc).__name__}: {exc}", fatal=True)

        return RunSummary(
            seen=seen,
            new_items=new_items,
            deduped=deduped,
            errored=errored,
            skipped=skipped,
        )

    # ------------------------------------------------------------------
    # Internals
    # ------------------------------------------------------------------

    async def _config(self) -> dict[str, Any]:
        """Read the plugin's config from the Hello handshake. Used by
        ListCollections which doesn't get a HostClient (the SDK only
        passes one on Sync today).
        """

        return getattr(self, "_dr_config", None) or {}


# ----- module-level helpers (keep Plugin class slim) ------------------


async def _config_from_host(host: HostClient) -> dict[str, Any]:
    """Pull the freshest config from the host.

    Sync reaches in here rather than reading ``self._dr_config`` so a
    SetConfig that happened between the Hello and this Sync is honoured.
    """

    return await host.get_config()


def _creds_from_config(cfg: dict[str, Any]) -> dict[str, Any]:
    """Convert the config blob into the creds dict the iterator expects."""

    source = cfg.get("source")
    if source == "web_api":
        return {
            "api_key": cfg.get("api_key", ""),
            "library_type": cfg.get("library_type", "user"),
            "library_id": cfg.get("library_id", ""),
        }
    # sqlite_upload (the catch-all path):
    return {
        "sqlite_path": cfg.get("sqlite_path", ""),
        "storage_dir": cfg.get("storage_dir"),
    }


async def _process_item(
    *,
    host: HostClient,
    run_id: str,
    creds: dict[str, Any],
    normalized: ZoteroItemNormalized,
    raw: dict[str, Any],
) -> str:
    """Per-item pipeline. Returns ``"duplicate"`` or ``"new"``.

    Failure modes (bubbled to the caller as exceptions): host RPC errors,
    PDF copy errors. The caller catches and stamps an item-level
    SyncError.
    """

    z_user_id = zotero_user_id(creds, raw)

    # 1. Dedup.
    hit = await find_existing(normalized, host=host)

    # 2. Stage the PDF if we have one locally and aren't a duplicate.
    document_id: str | None = None
    ingest_status = "duplicate" if hit is not None else "pending"
    ingest_error: str | None = None

    if hit is not None:
        document_id = hit.document_id
    else:
        # Fresh row -- pick the strongest ingest target by identifier.
        if normalized.arxiv_id:
            document_id = str(uuid.uuid4())
            ingest_status = "queued"
        elif normalized.pdf_local_path is not None:
            try:
                staged = await host.stage_path(
                    run_id=run_id,
                    filename=f"{normalized.zotero_key}.pdf",
                    content_type="application/pdf",
                )
                # Copy bytes into the host-provided shared-volume path so
                # the ingest worker (which runs on the host side) can read
                # them. Best-effort: if the source path vanishes we record
                # an item-level error instead of dropping the row.
                shutil.copyfile(normalized.pdf_local_path, staged)
                document_id = str(uuid.uuid4())
                ingest_status = "queued"
            except OSError as exc:
                logger.warning(
                    "zotero plugin: pdf stage failed for %s: %s",
                    normalized.zotero_key,
                    exc,
                )
                ingest_status = "errored"
                ingest_error = f"pdf stage failed: {exc}"
        elif normalized.doi:
            # Record DOI rows as queued -- a DOI-aware ingest path will
            # pick them up later. Matches the legacy behaviour.
            document_id = str(uuid.uuid4())
            ingest_status = "queued"
        else:
            ingest_status = "pending"
            ingest_error = "no usable ingest target (missing DOI/arxiv/PDF)"

    # 3. Persist the row via the host (canonical column shape).
    row = {
        "zotero_user_id": z_user_id,
        "zotero_key": normalized.zotero_key,
        "zotero_version": _safe_int(normalized.raw.get("version")) or 0,
        "canonical_id": normalized.canonical_id,
        "canonical_kind": normalized.canonical_kind,
        "doi": normalized.doi or "",
        "arxiv_id": normalized.arxiv_id or "",
        "isbn": normalized.isbn or "",
        # content_hash is bytes; Struct can't carry binary so we pass the
        # hex form. The host stores it as-is into the bytea column -- the
        # field is informational, not joined against.
        "content_hash": normalized.content_hash.hex() if normalized.content_hash else "",
        "title": normalized.title or "",
        "document_id": document_id or "",
        "ingest_status": ingest_status,
        "ingest_error": ingest_error or "",
        "metadata": {
            "raw_key": normalized.zotero_key,
            "raw_version": normalized.raw.get("version"),
            "item_type": normalized.item_type,
        },
    }
    await host.upsert_import_row(
        run_id=run_id,
        plugin_id="zotero",
        table="zotero_imports",
        row=row,
    )

    return "duplicate" if hit is not None else "new"


def _zotero_web_url(creds: dict[str, Any], item: ZoteroItemNormalized) -> str:
    """Best-effort permalink for the Zotero item.

    Web API mode has a stable URL; sqlite mode doesn't (the upload may
    have come from a library we have no id for). Fall back to an empty
    string -- the host treats that as "no external url".
    """

    library_type = creds.get("library_type")
    library_id = creds.get("library_id")
    if library_type and library_id and item.zotero_key:
        prefix = "users" if library_type == "user" else "groups"
        return f"https://www.zotero.org/{prefix}/{library_id}/items/{item.zotero_key}"
    return ""


def _safe_int(value: Any) -> int | None:
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


async def _list_web_api_collections(cfg: dict[str, Any]) -> list[Collection]:
    """Read the user's top-level Zotero collections.

    Best-effort: if the API is unreachable or auth fails we return an
    empty list rather than raising -- the caller sees zero collections
    and can still kick off a sync (which itself will fail loudly).
    """

    api_key = str(cfg.get("api_key") or "")
    library_type = str(cfg.get("library_type") or "user")
    library_id = str(cfg.get("library_id") or "")
    if not api_key or not library_id:
        return []

    # We don't pull pyzotero here (its collection API is sync); use the
    # client's raw HTTP to keep this async-clean.
    async with ZoteroClient(
        api_key=api_key,
        library_type=library_type,  # type: ignore[arg-type]
        library_id=library_id,
    ) as client:
        try:
            payload = await client._json(  # noqa: SLF001 - internal but stable
                "GET", f"{client.library_url}/collections", params={"limit": 100, "format": "json"}
            )
        except Exception:  # noqa: BLE001 - any client error -> empty list
            return []
    if not isinstance(payload, list):
        return []
    out: list[Collection] = []
    for entry in payload:
        if not isinstance(entry, dict):
            continue
        data = entry.get("data") or {}
        key = data.get("key") or entry.get("key")
        name = data.get("name") or ""
        item_count = entry.get("meta", {}).get("numItems", 0) if isinstance(entry.get("meta"), dict) else 0
        if not key:
            continue
        out.append(Collection(id=str(key), name=str(name), item_count=int(item_count)))
    return out
