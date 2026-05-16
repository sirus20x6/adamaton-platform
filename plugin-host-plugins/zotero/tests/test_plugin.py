"""Wiring tests for the Zotero plugin.

We don't run a real sync (needs creds + network); the goal is to verify
the SDK's @plugin / @importer decorators actually registered the
handlers and that the surrounding helpers (config -> creds mapping,
normalize -> upsert row shape) behave as expected.
"""

from __future__ import annotations

from dr_plugin_sdk.types import Collection, PluginItem, RunSummary, SyncArgs

from dr_plugin_zotero.normalize import ZoteroItemNormalized, normalize_item
from dr_plugin_zotero.plugin import (
    Plugin,
    _creds_from_config,
    _process_item,
    _zotero_web_url,
)


# asyncio_mode=auto (pyproject.toml) auto-marks async tests; no explicit
# pytestmark needed, and applying one to the whole module would attach
# the asyncio marker to plain sync tests (a noisy warning under pytest 8).


def test_plugin_class_registers_importer_handlers() -> None:
    """@plugin walked the class and stashed the rpc -> method map."""

    registry = getattr(Plugin, "_dr_rpc_registry", {})
    assert registry, "no rpc registry; @plugin didn't run"
    # The list_collections + sync decorators map to canonical RPC names.
    assert registry.get("ListCollections") == "list"
    assert registry.get("Sync") == "sync"


def test_plugin_capabilities_include_importer() -> None:
    """Capabilities surface in the Hello response; they should match the manifest."""

    caps = getattr(Plugin, "_dr_capabilities", [])
    assert "importer.list_collections" in caps
    assert "importer.sync" in caps


def test_creds_from_config_web_api() -> None:
    creds = _creds_from_config(
        {
            "source": "web_api",
            "api_key": "k",
            "library_type": "user",
            "library_id": "42",
        }
    )
    assert creds == {"api_key": "k", "library_type": "user", "library_id": "42"}


def test_creds_from_config_sqlite() -> None:
    creds = _creds_from_config(
        {"source": "sqlite_upload", "sqlite_path": "/tmp/z.sqlite", "storage_dir": "/tmp/s"}
    )
    assert creds == {"sqlite_path": "/tmp/z.sqlite", "storage_dir": "/tmp/s"}


def test_zotero_web_url_builds_user_link() -> None:
    item = ZoteroItemNormalized(
        zotero_key="ABCD1234",
        title="t",
        year=2024,
        doi=None,
        arxiv_id=None,
        isbn=None,
        content_hash=b"",
        item_type="journalArticle",
        has_pdf=False,
    )
    url = _zotero_web_url({"library_type": "user", "library_id": "42"}, item)
    assert url == "https://www.zotero.org/users/42/items/ABCD1234"


def test_zotero_web_url_empty_without_library_id() -> None:
    item = ZoteroItemNormalized(
        zotero_key="ABCD1234",
        title="t",
        year=None,
        doi=None,
        arxiv_id=None,
        isbn=None,
        content_hash=b"",
        item_type="journalArticle",
        has_pdf=False,
    )
    # sqlite_upload creds without library_id -> no external URL.
    assert _zotero_web_url({}, item) == ""


def test_normalize_item_round_trip_into_upsert_row_shape() -> None:
    """The plugin builds an upsert_import_row payload from a normalised item.

    We construct a representative envelope, normalise, and assert the
    fields the plugin will hand to the host all read cleanly. This is a
    pure-data test -- no host calls.
    """

    envelope = {
        "key": "ABCD1234",
        "version": 7,
        "data": {
            "itemType": "journalArticle",
            "title": "A Paper",
            "DOI": "10.1234/abc.def",
            "creators": [
                {"creatorType": "author", "firstName": "A.", "lastName": "Smith"},
            ],
            "date": "2024-01-15",
        },
        "library": {"type": "user", "id": 42},
    }
    norm = normalize_item(envelope)
    assert norm.doi == "10.1234/abc.def"
    assert norm.canonical_kind == "doi"
    # _process_item will hex-encode this; just verify the hash is non-empty.
    assert len(norm.content_hash) == 32


# ----- _process_item with a fake host -----------------------------------


class _FakeHost:
    """Minimal HostClient stand-in: records calls, returns scripted answers."""

    def __init__(self, *, known: bool = False, document_id: str = "") -> None:
        self._known = known
        self._document_id = document_id
        self.is_known_calls: list[tuple[str, str]] = []
        self.upsert_calls: list[dict] = []
        self.stage_calls: list[dict] = []

    async def is_known(self, plugin_id: str, external_id: str):
        self.is_known_calls.append((plugin_id, external_id))
        return self._known, self._document_id

    async def upsert_import_row(self, run_id: str, plugin_id: str, table: str, row: dict) -> str:
        self.upsert_calls.append(
            {"run_id": run_id, "plugin_id": plugin_id, "table": table, "row": row}
        )
        return "fake-id"

    async def stage_path(self, run_id: str, filename: str, content_type: str = "") -> str:
        self.stage_calls.append(
            {"run_id": run_id, "filename": filename, "content_type": content_type}
        )
        return f"/tmp/{filename}"


async def test_process_item_dedup_path_emits_duplicate() -> None:
    host = _FakeHost(known=True, document_id="11111111-1111-1111-1111-111111111111")
    envelope = {
        "key": "DUP",
        "version": 1,
        "data": {"itemType": "journalArticle", "title": "t", "DOI": "10.1234/dup"},
        "library": {"type": "user", "id": 1},
    }
    norm = normalize_item(envelope)
    outcome = await _process_item(
        host=host,  # type: ignore[arg-type]
        run_id="run1",
        creds={"library_type": "user", "library_id": "1"},
        normalized=norm,
        raw=envelope,
    )
    assert outcome == "duplicate"
    assert host.is_known_calls and host.is_known_calls[0][0] == "zotero"
    # Even on dedup we still upsert the row so ingest_status flips to
    # "duplicate" and the run accounting is accurate.
    assert host.upsert_calls
    last = host.upsert_calls[-1]["row"]
    assert last["ingest_status"] == "duplicate"
    assert last["document_id"] == "11111111-1111-1111-1111-111111111111"


async def test_process_item_new_doi_only_queues_without_stage() -> None:
    """An item with a DOI but no local PDF should queue without staging."""
    host = _FakeHost(known=False)
    envelope = {
        "key": "NEW1",
        "version": 1,
        "data": {"itemType": "journalArticle", "title": "t", "DOI": "10.1234/new"},
        "library": {"type": "user", "id": 1},
    }
    norm = normalize_item(envelope)
    outcome = await _process_item(
        host=host,  # type: ignore[arg-type]
        run_id="run1",
        creds={"library_type": "user", "library_id": "1"},
        normalized=norm,
        raw=envelope,
    )
    assert outcome == "new"
    assert not host.stage_calls, "no PDF -> no stage call"
    row = host.upsert_calls[-1]["row"]
    assert row["ingest_status"] == "queued"
    assert row["doi"] == "10.1234/new"


# ----- Smoke: the SDK types are importable + a PluginItem is sane ------


def test_plugin_item_type_smoke() -> None:
    """Sanity check that the dataclass we'll emit is well-formed."""

    item = PluginItem(
        plugin_id="zotero",
        external_id="ABCD",
        title="t",
        markdown_body="",
        metadata={"k": "v"},
    )
    proto = item.to_proto()
    assert proto.plugin_id == "zotero"
    assert proto.external_id == "ABCD"
    # Round-trip a RunSummary so we know the wire is wired.
    rs = RunSummary(seen=1, new_items=1, deduped=0)
    assert rs.to_proto().seen == 1


def test_collection_dataclass_smoke() -> None:
    """Make sure the Collection we hand back from ``list`` round-trips."""

    c = Collection(id="all", name="All items")
    assert c.to_proto().id == "all"


def test_sync_args_from_request_round_trip_shape() -> None:
    """SyncArgs is what the SDK gives us; verify .options access works."""

    # Build a minimal proto-shaped object so from_request doesn't choke.
    from google.protobuf import struct_pb2

    class _Req:
        run_id = "run-123"
        collection_id = ""
        since = ""
        corpus_id = ""
        options = struct_pb2.Struct()

    req = _Req()
    req.options.update({"only_with_pdf": True, "since": 42})
    args = SyncArgs.from_request(req)
    assert args.run_id == "run-123"
    assert args.options.get("only_with_pdf") is True
    assert int(args.options.get("since")) == 42
