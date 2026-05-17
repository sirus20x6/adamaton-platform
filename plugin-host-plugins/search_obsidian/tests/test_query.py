"""Smoke test for the Obsidian vault search plugin.

Uses a ``tmp_path`` fixture to seed a fake vault with two markdown notes
(one with YAML frontmatter, one without) plus a ``.obsidian/`` config
folder that must be skipped. No network access.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_obsidian.adapter import ObsidianAdapter
from dr_plugin_search_obsidian.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


def _seed_vault(root) -> None:
    """Populate ``root`` with a minimal fake vault."""
    # Note 1: has YAML frontmatter with title / author / date.
    (root / "with-frontmatter.md").write_text(
        "---\n"
        "title: The Attention Note\n"
        "author: Ada Lovelace\n"
        "date: 2024-03-14\n"
        "tags: [ml, transformers]\n"
        "---\n"
        "\n"
        "Discussion of attention mechanisms and transformers in modern NLP.\n",
        encoding="utf-8",
    )
    # Note 2: no frontmatter — title falls back to file basename.
    (root / "plain-note.md").write_text(
        "Plain markdown about apples and bananas. No frontmatter here.\n",
        encoding="utf-8",
    )
    # `.obsidian/` config folder — must be ignored entirely.
    obsidian_dir = root / ".obsidian"
    obsidian_dir.mkdir()
    (obsidian_dir / "config.md").write_text(
        "This file contains the word attention but lives in .obsidian/.\n",
        encoding="utf-8",
    )
    # Subdirectory note — must be discovered.
    sub = root / "notes" / "deep"
    sub.mkdir(parents=True)
    (sub / "nested.md").write_text(
        "Nested note mentioning attention deep in the vault.\n",
        encoding="utf-8",
    )


async def test_query_finds_body_match(tmp_path) -> None:
    _seed_vault(tmp_path)
    plugin = Plugin(vault_path=str(tmp_path))

    page = await plugin.query("attention", 10, None, None)

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)

    titles = {r.title for r in page.results}
    # The frontmatter note's title comes from frontmatter, not the filename.
    assert "The Attention Note" in titles
    # The nested note's title falls back to the file basename.
    assert "nested" in titles
    # `.obsidian/` config-folder content must not surface as a hit.
    assert not any(".obsidian" in r.raw.get("rel_path", "") for r in page.results)

    for r in page.results:
        assert r.source_kind == SourceKind.WIKI
        assert r.adapter == "obsidian"
        assert r.external_id
        assert r.url.startswith("file://")


async def test_frontmatter_metadata_propagated(tmp_path) -> None:
    _seed_vault(tmp_path)
    plugin = Plugin(vault_path=str(tmp_path))

    page = await plugin.query("attention", 10, None, None)
    by_title = {r.title: r for r in page.results}

    hit = by_title["The Attention Note"]
    assert hit.authors == ["Ada Lovelace"]
    # `date: 2024-03-14` in frontmatter should win over file mtime.
    assert hit.published_at is not None
    assert hit.published_at.year == 2024
    assert hit.published_at.month == 3
    assert hit.published_at.day == 14
    # venue is the vault directory name.
    assert hit.venue == os.path.basename(str(tmp_path))
    # raw includes the original frontmatter dict (json-safe).
    assert hit.raw["frontmatter"]["title"] == "The Attention Note"
    assert hit.raw["frontmatter"]["tags"] == ["ml", "transformers"]


async def test_plain_note_without_frontmatter(tmp_path) -> None:
    _seed_vault(tmp_path)
    plugin = Plugin(vault_path=str(tmp_path))

    page = await plugin.query("bananas", 10, None, None)
    titles = [r.title for r in page.results]
    assert "plain-note" in titles
    hit = next(r for r in page.results if r.title == "plain-note")
    # No author metadata when no frontmatter.
    assert hit.authors == []
    # published_at falls back to file mtime.
    assert hit.published_at is not None


async def test_pagination_offset_cursor(tmp_path) -> None:
    _seed_vault(tmp_path)
    # Add a few more matching notes so pagination is meaningful.
    for i in range(5):
        (tmp_path / f"extra-{i}.md").write_text(
            f"Extra note {i} mentioning attention.\n", encoding="utf-8"
        )

    plugin = Plugin(vault_path=str(tmp_path))

    first = await plugin.query("attention", 2, None, None)
    assert len(first.results) == 2
    assert first.next_cursor == "2"

    second = await plugin.query("attention", 2, first.next_cursor, None)
    assert len(second.results) == 2
    # No overlap between pages.
    first_ids = {r.external_id for r in first.results}
    second_ids = {r.external_id for r in second.results}
    assert first_ids.isdisjoint(second_ids)


async def test_exclude_folders_override(tmp_path) -> None:
    _seed_vault(tmp_path)
    # Add a "drafts" folder; default excludes shouldn't skip it.
    drafts = tmp_path / "drafts"
    drafts.mkdir()
    (drafts / "draft.md").write_text(
        "Draft about attention.\n", encoding="utf-8"
    )

    # Pass a custom exclude list that drops the drafts folder.
    plugin = Plugin(vault_path=str(tmp_path), exclude_folders=["drafts", ".obsidian"])
    page = await plugin.query("attention", 10, None, None)
    rel_paths = {r.raw["rel_path"] for r in page.results}
    assert not any(p.startswith("drafts/") for p in rel_paths)


async def test_adapter_missing_vault_returns_empty(tmp_path) -> None:
    """An unconfigured / missing vault is not a crash — just empty results."""
    missing = tmp_path / "nope"
    adapter = ObsidianAdapter(vault_path=str(missing))
    page = await adapter.search("anything", limit=5, cursor=None, since=None)
    assert isinstance(page, SearchPage)
    assert page.results == []
    assert page.next_cursor == ""
