"""Local filesystem search adapter.

Walks a configured directory at query time and substring-greps files with
allowed extensions. No persistent index — for v0.1.0 the directory is small
enough that walking on every query is fine.

Configuration is taken from the constructor (``root_path``,
``include_extensions``, ``max_file_bytes``). The :class:`Plugin` wrapper
reads ``LOCALFS_ROOT`` from the environment to seed ``root_path`` when the
adapter is constructed from the manifest — the env-var route was chosen
over reading the manifest's ``config_schema.defaults`` because the
plugin-host already materializes per-plugin config into the process env.
Tests inject ``root_path`` directly via the constructor.
"""

from __future__ import annotations

import asyncio
import hashlib
import os
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind

DEFAULT_INCLUDE_EXTENSIONS: tuple[str, ...] = (".md", ".txt", ".pdf")
DEFAULT_MAX_FILE_BYTES = 5_000_000
SNIPPET_RADIUS = 100  # chars on each side of first match -> ~200 char abstract


class LocalFSAdapter:
    name = "localfs"
    source_kind = SourceKind.WIKI

    def __init__(
        self,
        root_path: str | os.PathLike[str] | None = None,
        *,
        include_extensions: list[str] | tuple[str, ...] | None = None,
        max_file_bytes: int | None = None,
    ) -> None:
        if root_path is None:
            raise ValueError(
                "LocalFSAdapter requires root_path (constructor arg or "
                "LOCALFS_ROOT env var via Plugin wrapper)."
            )
        self._root = Path(root_path).expanduser().resolve()
        exts = include_extensions or DEFAULT_INCLUDE_EXTENSIONS
        self._exts = tuple(e.lower() if e.startswith(".") else f".{e.lower()}" for e in exts)
        self._max_bytes = int(max_file_bytes) if max_file_bytes else DEFAULT_MAX_FILE_BYTES

    # --- file text extraction -------------------------------------------------

    def _read_text(self, path: Path) -> str:
        suffix = path.suffix.lower()
        if suffix == ".pdf":
            return self._read_pdf(path)
        # .md / .txt / anything else allow-listed is treated as utf-8 text.
        try:
            return path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return ""

    @staticmethod
    def _read_pdf(path: Path) -> str:
        try:
            from pypdf import PdfReader  # imported lazily so tests w/o PDFs don't need it
        except ImportError:
            return ""
        try:
            reader = PdfReader(str(path))
        except Exception:
            return ""
        chunks: list[str] = []
        for page in reader.pages:
            try:
                chunks.append(page.extract_text() or "")
            except Exception:
                continue
        return "\n".join(chunks)

    # --- result construction --------------------------------------------------

    @staticmethod
    def _external_id_for(path: Path) -> str:
        return hashlib.sha256(str(path).encode("utf-8")).hexdigest()[:16]

    @staticmethod
    def _snippet(text: str, needle_lower: str) -> str:
        if not needle_lower:
            return text[: 2 * SNIPPET_RADIUS]
        idx = text.lower().find(needle_lower)
        if idx < 0:
            return text[: 2 * SNIPPET_RADIUS]
        start = max(0, idx - SNIPPET_RADIUS)
        end = min(len(text), idx + len(needle_lower) + SNIPPET_RADIUS)
        snippet = text[start:end]
        # Collapse whitespace so abstracts don't blow up with newlines.
        return " ".join(snippet.split())

    def _to_result(self, path: Path, text: str, query_lower: str, count: int) -> SearchResult:
        try:
            stat = path.stat()
        except OSError:
            size = 0
            mtime: datetime | None = None
        else:
            size = stat.st_size
            mtime = datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc)

        abs_path = str(path)
        return SearchResult(
            adapter=self.name,
            external_id=self._external_id_for(path),
            title=path.name,
            url=f"file://{abs_path}",
            abstract=self._snippet(text, query_lower),
            authors=[],
            published_at=mtime,
            venue="local filesystem",
            citation_count=0,
            raw={"path": abs_path, "size": size},
            score=float(count),
            source_kind=SourceKind.WIKI,
        )

    # --- query ----------------------------------------------------------------

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        limit = max(int(limit), 1)

        loop = asyncio.get_running_loop()
        all_hits = await loop.run_in_executor(None, self._collect_hits, query, since)

        # Sort by occurrence count desc, then path for determinism.
        all_hits.sort(key=lambda r: (-r.score, r.raw.get("path", "")))

        sliced = all_hits[offset : offset + limit]
        next_cursor = str(offset + len(sliced)) if len(all_hits) > offset + limit else ""

        return SearchPage(
            results=sliced,
            next_cursor=next_cursor,
            total_estimated=len(all_hits),
        )

    def _collect_hits(self, query: str, since: datetime | None) -> list[SearchResult]:
        if not self._root.exists() or not self._root.is_dir():
            return []
        needle = (query or "").lower()
        results: list[SearchResult] = []
        for dirpath, _dirnames, filenames in os.walk(self._root):
            for fname in filenames:
                p = Path(dirpath) / fname
                if p.suffix.lower() not in self._exts:
                    continue
                try:
                    st = p.stat()
                except OSError:
                    continue
                if st.st_size > self._max_bytes:
                    continue
                if since is not None:
                    mtime = datetime.fromtimestamp(st.st_mtime, tz=timezone.utc)
                    if mtime < since:
                        continue
                text = self._read_text(p)
                if not text:
                    continue
                if needle:
                    count = text.lower().count(needle)
                    if count == 0:
                        continue
                else:
                    # Empty query: include everything, score by size as a fallback.
                    count = 1
                results.append(self._to_result(p, text, needle, count))
        return results

    async def aclose(self) -> None:  # pragma: no cover - nothing to close
        return None


__all__ = ["LocalFSAdapter"]
