"""Logseq graph adapter.

Logseq stores a graph as a directory tree of plain markdown files:

    <graph_root>/
        pages/                  regular pages, one .md file per page
            Some Page.md
            another-page.md
        journals/               daily journal entries
            2024_01_15.md       filename convention: YYYY_MM_DD.md (underscores)
        assets/                 binary attachments (skipped)
        logseq/                 graph config + version-control metadata (skipped)

Journal filenames use **underscores** (not dashes), e.g. ``2024_01_15.md``
for the daily note from 15 January 2024. We parse that filename to obtain
the entry's ``published_at``; for regular pages we fall back to the file
mtime. ``since`` is applied against whichever date we used.

The search is a simple case-insensitive substring scan across each file's
contents. Matches are sorted by recency (newest first) and paginated by
integer offset encoded as the cursor.
"""

from __future__ import annotations

import asyncio
import hashlib
import os
import re
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


# Journal filenames look like 2024_01_15.md (underscored YYYY_MM_DD).
_JOURNAL_FILENAME_RE = re.compile(r"^(\d{4})_(\d{2})_(\d{2})$")

# Top-level directories Logseq uses for non-content; never walk into these.
_SKIP_DIRS = {"assets", "logseq", ".git", ".obsidian", ".trash"}

# Snippet window around a match, in characters.
_SNIPPET_RADIUS = 120


@dataclass(slots=True)
class _Hit:
    path: Path
    is_journal: bool
    published_at: datetime
    title: str
    snippet: str


class LogseqAdapter:
    name = "logseq"

    def __init__(self, *, graph_path: str, include_journals: bool = True) -> None:
        self._graph_path = Path(graph_path) if graph_path else None
        self._include_journals = include_journals

    # ----- helpers --------------------------------------------------------

    @staticmethod
    def _parse_journal_date(stem: str) -> datetime | None:
        """Parse a journal filename stem like ``2024_01_15`` into a datetime."""
        m = _JOURNAL_FILENAME_RE.match(stem)
        if not m:
            return None
        try:
            year, month, day = (int(x) for x in m.groups())
            return datetime(year, month, day, tzinfo=timezone.utc)
        except ValueError:
            return None

    @staticmethod
    def _extract_title(text: str, fallback: str) -> str:
        """First ``# heading`` if present, else filename-derived fallback."""
        for line in text.splitlines():
            stripped = line.strip()
            if stripped.startswith("# "):
                return stripped[2:].strip() or fallback
            # Stop scanning after a handful of leading lines; Logseq pages
            # tend to put the title on line 1 if at all.
            if stripped and not stripped.startswith("#"):
                break
        return fallback

    @staticmethod
    def _make_snippet(text: str, needle_lower: str) -> str:
        if not needle_lower:
            return text[: _SNIPPET_RADIUS * 2].strip()
        idx = text.lower().find(needle_lower)
        if idx < 0:
            return text[: _SNIPPET_RADIUS * 2].strip()
        start = max(0, idx - _SNIPPET_RADIUS)
        end = min(len(text), idx + len(needle_lower) + _SNIPPET_RADIUS)
        snippet = text[start:end].strip()
        if start > 0:
            snippet = "..." + snippet
        if end < len(text):
            snippet = snippet + "..."
        return snippet

    def _iter_markdown_files(self) -> list[Path]:
        """Walk the graph root and yield candidate .md files."""
        if self._graph_path is None or not self._graph_path.is_dir():
            return []

        out: list[Path] = []
        root = self._graph_path
        for dirpath, dirnames, filenames in os.walk(root):
            # Prune skip dirs in-place so os.walk doesn't recurse into them.
            dirnames[:] = [d for d in dirnames if d not in _SKIP_DIRS]
            rel = Path(dirpath).relative_to(root)
            top = rel.parts[0] if rel.parts else ""
            if top == "journals" and not self._include_journals:
                # Skip the entire journals subtree.
                dirnames[:] = []
                continue
            for name in filenames:
                if name.endswith(".md"):
                    out.append(Path(dirpath) / name)
        return out

    def _scan_file(self, path: Path, needle_lower: str) -> _Hit | None:
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return None
        if needle_lower and needle_lower not in text.lower():
            return None

        assert self._graph_path is not None
        try:
            rel = path.relative_to(self._graph_path)
        except ValueError:
            return None

        top = rel.parts[0] if rel.parts else ""
        is_journal = top == "journals"
        stem = path.stem

        published_at: datetime | None = None
        if is_journal:
            published_at = self._parse_journal_date(stem)
        if published_at is None:
            try:
                published_at = datetime.fromtimestamp(
                    path.stat().st_mtime, tz=timezone.utc
                )
            except OSError:
                published_at = datetime.now(tz=timezone.utc)

        title = self._extract_title(text, fallback=stem)
        snippet = self._make_snippet(text, needle_lower)

        return _Hit(
            path=path,
            is_journal=is_journal,
            published_at=published_at,
            title=title,
            snippet=snippet,
        )

    def _to_result(self, hit: _Hit) -> SearchResult:
        assert self._graph_path is not None
        rel = hit.path.relative_to(self._graph_path)
        rel_str = str(rel)
        digest = hashlib.sha1(rel_str.encode("utf-8")).hexdigest()[:16]
        venue = self._graph_path.name or self._graph_path.as_posix()
        raw: dict[str, Any] = {
            "path": str(hit.path.resolve()),
            "is_journal": hit.is_journal,
        }
        return SearchResult(
            adapter=self.name,
            external_id=digest,
            title=hit.title,
            url=f"file://{hit.path.resolve()}",
            abstract=hit.snippet,
            published_at=hit.published_at,
            venue=venue,
            raw=raw,
            source_kind=SourceKind.WIKI,
        )

    # ----- public API -----------------------------------------------------

    def _search_sync(
        self,
        query: str,
        *,
        limit: int,
        offset: int,
        since: datetime | None,
    ) -> SearchPage:
        if self._graph_path is None or not self._graph_path.is_dir():
            return SearchPage(results=[], next_cursor="", total_estimated=0)

        needle_lower = query.lower().strip()

        # Normalize `since` to a tz-aware datetime so comparisons are safe.
        since_aware: datetime | None = None
        if since is not None:
            since_aware = (
                since if since.tzinfo is not None else since.replace(tzinfo=timezone.utc)
            )

        hits: list[_Hit] = []
        for path in self._iter_markdown_files():
            hit = self._scan_file(path, needle_lower)
            if hit is None:
                continue
            if since_aware is not None and hit.published_at < since_aware:
                continue
            hits.append(hit)

        # Sort by recency, newest first.
        hits.sort(key=lambda h: h.published_at, reverse=True)

        total = len(hits)
        sliced = hits[offset : offset + max(limit, 0)]
        results = [self._to_result(h) for h in sliced]
        next_cursor = str(offset + len(sliced)) if offset + len(sliced) < total else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(
            None,
            lambda: self._search_sync(
                query,
                limit=max(int(limit), 0),
                offset=offset,
                since=since,
            ),
        )

    async def aclose(self) -> None:
        return None
