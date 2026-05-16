"""Dedup helper that asks the host instead of poking Postgres directly.

The legacy ``app.zotero.dedup`` queried both ``platform.zotero_imports``
and the R2R documents table through SQLAlchemy. In the plugin world the
plugin has no DB credentials -- the host's ``IsKnown`` RPC is the only
way to learn whether an external id was already ingested.

We probe both the canonical id (DOI / arXiv id / ISBN / content hash)
and the raw zotero key; the host's zotero_imports lookup matches on
either, so a single call covers what the prior two-step lookup did.
"""

from __future__ import annotations

from dataclasses import dataclass

from dr_plugin_sdk.host_client import HostClient

from .normalize import ZoteroItemNormalized


@dataclass(slots=True, kw_only=True)
class DedupHit:
    """Result of a successful dedup probe -- the resolved document id."""

    document_id: str | None
    matched_id: str


async def find_existing(
    item: ZoteroItemNormalized,
    *,
    host: HostClient,
) -> DedupHit | None:
    """Ask the host if ``item`` was already imported.

    Two probes per item in the worst case (canonical id, then zotero
    key) -- usually only the first is needed. The host's IsKnown matches
    either form against ``platform.zotero_imports`` so a hit on either
    counts as a duplicate.
    """

    # Strongest identifier first.
    known, doc_id = await host.is_known(plugin_id="zotero", external_id=item.canonical_id)
    if known:
        return DedupHit(document_id=doc_id or None, matched_id=item.canonical_id)

    # Fall back to the zotero key when the canonical id differs (e.g. a
    # zotero_only row whose canonical id is "zotero:<key>" -- a prior
    # import might have ended up with a different canonical kind once a
    # DOI was added).
    if item.zotero_key and item.zotero_key != item.canonical_id:
        known, doc_id = await host.is_known(plugin_id="zotero", external_id=item.zotero_key)
        if known:
            return DedupHit(document_id=doc_id or None, matched_id=item.zotero_key)

    return None


__all__ = ["DedupHit", "find_existing"]
