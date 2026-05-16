"""Round-trip dataclass <-> proto for every type the SDK exposes."""

from __future__ import annotations

from datetime import datetime, timezone

from dr_plugin_sdk.types import (
    Collection,
    FetchedDoc,
    PluginItem,
    RunSummary,
    SearchPage,
    SearchResult,
    SourceKind,
)


def test_plugin_item_roundtrip() -> None:
    item = PluginItem(
        plugin_id="zotero",
        external_id="ZOT-1",
        external_url="https://example.com/1",
        title="Hello",
        markdown_body="# Hello",
        metadata={"k": "v", "n": 3},
        attachments=["/tmp/a.pdf", "/tmp/b.png"],
    )
    proto = item.to_proto()
    back = PluginItem.from_proto(proto)
    assert back.plugin_id == "zotero"
    assert back.external_id == "ZOT-1"
    assert back.external_url == "https://example.com/1"
    assert back.title == "Hello"
    assert back.markdown_body == "# Hello"
    # struct round-trip turns ints into floats; just check the keys + str(value).
    assert set(back.metadata.keys()) == {"k", "n"}
    assert back.metadata["k"] == "v"
    assert back.attachments == ["/tmp/a.pdf", "/tmp/b.png"]


def test_collection_roundtrip() -> None:
    col = Collection(id="c1", name="My Coll", item_count=42, metadata={"owner": "me"})
    back = Collection.from_proto(col.to_proto())
    assert back.id == "c1"
    assert back.name == "My Coll"
    assert back.item_count == 42
    assert back.metadata == {"owner": "me"}


def test_search_result_roundtrip() -> None:
    when = datetime(2026, 5, 15, 12, 0, 0, tzinfo=timezone.utc)
    r = SearchResult(
        adapter="arxiv",
        external_id="2401.0001",
        title="A paper",
        url="https://arxiv.org/abs/2401.0001",
        abstract="abstract",
        authors=["A", "B"],
        published_at=when,
        venue="arXiv",
        citation_count=7,
        raw={"foo": "bar"},
        score=0.91,
        source_kind=SourceKind.PREPRINT,
    )
    back = SearchResult.from_proto(r.to_proto())
    assert back.adapter == "arxiv"
    assert back.external_id == "2401.0001"
    assert back.authors == ["A", "B"]
    assert back.published_at == when
    assert back.citation_count == 7
    assert back.source_kind == SourceKind.PREPRINT
    assert back.score == 0.91


def test_search_page_roundtrip() -> None:
    page = SearchPage(
        results=[SearchResult(external_id="x", title="t")],
        next_cursor="cur",
        total_estimated=100,
    )
    back = SearchPage.from_proto(page.to_proto())
    assert len(back.results) == 1
    assert back.results[0].external_id == "x"
    assert back.next_cursor == "cur"
    assert back.total_estimated == 100


def test_fetched_doc_roundtrip() -> None:
    body = b"\x89PNG\x00binarystuff"
    d = FetchedDoc(
        adapter="arxiv",
        external_id="2401.0001",
        url="https://example.com",
        title="t",
        content_type="application/pdf",
        body=body,
        source_tier="pdf",
        metadata={"pages": 12},
    )
    back = FetchedDoc.from_proto(d.to_proto())
    assert back.body == body
    assert back.content_type == "application/pdf"
    assert back.source_tier == "pdf"


def test_run_summary_roundtrip() -> None:
    s = RunSummary(seen=10, new_items=4, fetched=4, deduped=6, queued=3, errored=1, skipped=0)
    back = RunSummary.from_proto(s.to_proto())
    assert back == s
