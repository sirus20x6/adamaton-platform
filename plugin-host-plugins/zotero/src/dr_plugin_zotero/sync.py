"""Zotero item iterator.

This is the pure-iteration slice of the legacy ``app.zotero.sync``
module: source dispatch, PDF-path resolution, and the per-item async
generator. Everything DB-touching (dedup, run-row persistence, totals
flushing) moved out -- the plugin entry-point now drives that via
``HostClient``.

Public surface:

* :func:`iter_items` — async iterator yielding ``(raw, normalized)``
  tuples for either source.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any, AsyncIterator, Iterator, Literal

from .client import ZoteroClient
from .normalize import ZoteroItemNormalized, normalize_item


async def iter_items(
    source: Literal["web_api", "sqlite_upload"],
    creds: dict[str, Any],
    *,
    only_with_pdf: bool = False,
    since: int | None = None,
) -> AsyncIterator[tuple[dict[str, Any], ZoteroItemNormalized]]:
    """Yield ``(raw_envelope, normalized)`` for every item in the library.

    ``only_with_pdf`` is a fast-path filter applied here so the orchestrator
    (the @importer.sync handler) doesn't have to re-check ``has_pdf``.
    """

    if source == "web_api":
        async for raw, normalized in _iter_web_api(creds, since=since):
            if only_with_pdf and not normalized.has_pdf:
                continue
            yield raw, normalized
    elif source == "sqlite_upload":
        async for raw, normalized in _iter_sqlite(creds):
            if only_with_pdf and not normalized.has_pdf:
                continue
            yield raw, normalized
    else:
        raise ValueError(f"unknown source: {source!r}")


async def _iter_web_api(
    creds: dict[str, Any],
    *,
    since: int | None,
) -> AsyncIterator[tuple[dict[str, Any], ZoteroItemNormalized]]:
    api_key = str(creds.get("api_key") or "")
    library_type = str(creds.get("library_type") or "user")
    library_id = str(creds.get("library_id") or "")

    async with ZoteroClient(
        api_key=api_key,
        library_type=library_type,  # type: ignore[arg-type]
        library_id=library_id,
    ) as client:
        async for envelope in client.iter_items(since=since):
            yield envelope, normalize_item(envelope)


async def _iter_sqlite(
    creds: dict[str, Any],
) -> AsyncIterator[tuple[dict[str, Any], ZoteroItemNormalized]]:
    """Wrap the sync :func:`read_items` iterator as an async generator.

    sqlite I/O happens on the calling thread; we yield to the event loop
    between items so the plugin keeps responding to gRPC keepalives.
    """

    from .sqlite_reader import read_items  # local import (sqlite-only path)

    sqlite_path = Path(str(creds.get("sqlite_path") or ""))
    storage_dir = creds.get("storage_dir")
    storage_root = Path(str(storage_dir)) if storage_dir else None

    iterator: Iterator[dict[str, Any]] = read_items(sqlite_path)

    while True:
        try:
            envelope = next(iterator)
        except StopIteration:
            return

        pdf_local_path: Path | None = None
        if storage_root is not None:
            data = envelope.get("data") or {}
            attachments = data.get("_pdf_attachments") or []
            if attachments:
                first = attachments[0]
                key = first.get("key")
                rel_path = first.get("path") or ""
                if key:
                    candidate = _resolve_storage_pdf(storage_root, key, rel_path)
                    if candidate is not None and candidate.exists():
                        pdf_local_path = candidate

        normalized = normalize_item(envelope, pdf_local_path=pdf_local_path)
        yield envelope, normalized
        await asyncio.sleep(0)


def _resolve_storage_pdf(
    storage_root: Path,
    item_key: str,
    rel_path: str,
) -> Path | None:
    """Resolve a Zotero attachment to a real path under ``storage_root``.

    Zotero stores the path field as either ``storage:filename.pdf``
    (canonical) or as a relative path like ``filename.pdf``. The actual
    bytes live at ``<storage_root>/<itemKey>/<filename>``.
    """

    cleaned = rel_path
    if cleaned.startswith("storage:"):
        cleaned = cleaned[len("storage:") :]
    cleaned = cleaned.lstrip("/")
    if not cleaned:
        cleaned = f"{item_key}.pdf"
    return storage_root / item_key / cleaned


def zotero_user_id(creds: dict[str, Any], raw: dict[str, Any]) -> str:
    """Build the ``"users/<id>"`` / ``"groups/<id>"`` key for ``zotero_imports``.

    Mirrors the legacy helper -- we want every row to land with the same
    library scope as before so dedup against existing rows works.
    """

    library_type = creds.get("library_type")
    library_id = creds.get("library_id")
    if library_type and library_id:
        prefix = "users" if library_type == "user" else "groups"
        return f"{prefix}/{library_id}"
    library = raw.get("library") if isinstance(raw, dict) else None
    if isinstance(library, dict):
        ltype = str(library.get("type") or "user")
        lid = library.get("id") or 0
        prefix = "users" if ltype == "user" else "groups"
        return f"{prefix}/{lid}"
    return "users/0"


__all__ = ["iter_items", "zotero_user_id"]
