"""Obsidian vault adapter.

Walks an on-disk Obsidian vault directory and substring-greps across each
note's body **and** YAML frontmatter values. Results are sorted by file
mtime (newest first) and paginated with an integer offset cursor.

Obsidian's conventions respected here:

* The ``.obsidian/`` directory at the vault root stores app/plugin config
  and is never treated as content — it is skipped by default along with
  ``.trash/`` (Obsidian's soft-delete folder). Callers can override
  ``exclude_folders`` via plugin config.
* Notes are plain UTF-8 markdown with optional YAML frontmatter parsed by
  ``python-frontmatter``.

The adapter does not currently parse Obsidian-specific syntax (``[[wiki
links]]``, callouts, dataview queries) — substring matching is on the raw
markdown body for v0.1.0.
"""

from __future__ import annotations

import asyncio
import hashlib
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

import frontmatter

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind

_ABSTRACT_LEN = 200
_DEFAULT_EXCLUDES = (".obsidian", ".trash")


@dataclass(slots=True)
class _Note:
    """In-memory view of a single matched note."""

    path: Path
    rel_path: str
    mtime: float
    title: str
    body: str
    metadata: dict[str, Any]
    match_index: int  # offset into body where the query matched (-1 = frontmatter only)


class ObsidianAdapter:
    name = "obsidian"
    source_kind = SourceKind.WIKI

    def __init__(
        self,
        *,
        vault_path: str,
        exclude_folders: Iterable[str] | None = None,
    ) -> None:
        self._vault_path = vault_path
        self._exclude_folders = set(
            exclude_folders if exclude_folders is not None else _DEFAULT_EXCLUDES
        )

    # ------------------------------------------------------------------
    # public API
    # ------------------------------------------------------------------

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        offset = int(cursor) if cursor and cursor.lstrip("-").isdigit() else 0
        offset = max(offset, 0)
        limit = max(int(limit), 1)

        loop = asyncio.get_running_loop()
        notes = await loop.run_in_executor(
            None, self._scan_sync, query, since
        )

        sliced = notes[offset : offset + limit]
        results = [self._to_result(n) for n in sliced]
        next_cursor = (
            str(offset + len(sliced))
            if len(notes) > offset + limit
            else ""
        )
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=len(notes),
        )

    # ------------------------------------------------------------------
    # internals (sync — invoked via run_in_executor)
    # ------------------------------------------------------------------

    def _scan_sync(
        self,
        query: str,
        since: datetime | None,
    ) -> list[_Note]:
        root = Path(self._vault_path)
        if not root.is_dir():
            # Empty page is better than blowing up the worker; an
            # unconfigured vault is a user error, not a plugin crash.
            return []

        needle = query.lower()
        since_ts = (
            since.astimezone(timezone.utc).timestamp()
            if since is not None
            else None
        )

        matches: list[_Note] = []
        for md_path in self._walk(root):
            try:
                st = md_path.stat()
            except OSError:
                continue
            if since_ts is not None and st.st_mtime < since_ts:
                continue
            try:
                post = frontmatter.load(md_path)
            except Exception:
                # Malformed frontmatter / non-UTF-8 — fall back to raw read.
                try:
                    raw_text = md_path.read_text(
                        encoding="utf-8", errors="replace"
                    )
                except OSError:
                    continue
                post = frontmatter.Post(content=raw_text, **{})

            body = post.content or ""
            meta = dict(post.metadata or {})

            match_idx = self._match_index(body, meta, needle)
            if match_idx is None:
                continue

            rel = md_path.relative_to(root).as_posix()
            title = self._title(meta, md_path)
            matches.append(
                _Note(
                    path=md_path,
                    rel_path=rel,
                    mtime=st.st_mtime,
                    title=title,
                    body=body,
                    metadata=meta,
                    match_index=match_idx,
                )
            )

        matches.sort(key=lambda n: n.mtime, reverse=True)
        return matches

    def _walk(self, root: Path) -> Iterable[Path]:
        """Yield every ``.md`` file under ``root``, skipping excluded dirs.

        Skips by basename anywhere in the tree (``.obsidian``, ``.trash``)
        rather than only at the vault root, because Obsidian's
        ``.obsidian/`` invariant is documented as "config folder name"
        and users sometimes nest vaults.
        """
        excludes = self._exclude_folders
        stack: list[Path] = [root]
        while stack:
            current = stack.pop()
            try:
                entries = list(current.iterdir())
            except OSError:
                continue
            for entry in entries:
                name = entry.name
                if entry.is_dir():
                    if name in excludes:
                        continue
                    stack.append(entry)
                    continue
                if name.endswith(".md") and entry.is_file():
                    yield entry

    @staticmethod
    def _match_index(
        body: str, meta: dict[str, Any], needle: str
    ) -> int | None:
        """Return body-offset of match, ``-1`` for frontmatter-only, ``None`` if no match.

        Empty queries return ``0`` so callers can list everything (sorted
        by mtime) for vault browsing UIs.
        """
        if not needle:
            return 0
        idx = body.lower().find(needle)
        if idx != -1:
            return idx
        # Substring search across frontmatter values (stringified).
        for value in meta.values():
            if _stringify(value).lower().find(needle) != -1:
                return -1
        return None

    @staticmethod
    def _title(meta: dict[str, Any], path: Path) -> str:
        raw = meta.get("title")
        if isinstance(raw, str) and raw.strip():
            return raw.strip()
        return path.stem

    def _to_result(self, note: _Note) -> SearchResult:
        published_at = self._published_at(note.metadata, note.mtime)
        authors = _authors(note.metadata)
        abstract = self._abstract(note.body, note.match_index)

        external_id = hashlib.sha1(
            note.rel_path.encode("utf-8")
        ).hexdigest()
        url = "file://" + str(note.path.resolve())
        venue = Path(self._vault_path).resolve().name

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=note.title,
            url=url,
            abstract=abstract,
            authors=authors,
            published_at=published_at,
            venue=venue,
            raw={
                "rel_path": note.rel_path,
                "frontmatter": _json_safe(note.metadata),
                "mtime": note.mtime,
            },
            source_kind=SourceKind.WIKI,
        )

    @staticmethod
    def _abstract(body: str, match_index: int) -> str:
        if not body:
            return ""
        if match_index <= 0:
            return body[:_ABSTRACT_LEN].strip()
        # Window a snippet around the match.
        start = max(0, match_index - _ABSTRACT_LEN // 4)
        end = min(len(body), start + _ABSTRACT_LEN)
        snippet = body[start:end].strip()
        return snippet

    @staticmethod
    def _published_at(meta: dict[str, Any], mtime: float) -> datetime | None:
        raw = meta.get("date") or meta.get("created") or meta.get("published")
        parsed = _parse_dt(raw)
        if parsed is not None:
            return parsed
        try:
            return datetime.fromtimestamp(mtime, tz=timezone.utc)
        except (OverflowError, OSError, ValueError):
            return None


# ----------------------------------------------------------------------
# small free-function helpers
# ----------------------------------------------------------------------


def _stringify(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, (list, tuple)):
        return " ".join(_stringify(v) for v in value)
    if isinstance(value, dict):
        return " ".join(_stringify(v) for v in value.values())
    return str(value)


def _authors(meta: dict[str, Any]) -> list[str]:
    raw = meta.get("authors")
    if raw is None:
        raw = meta.get("author")
    if raw is None:
        return []
    if isinstance(raw, str):
        cleaned = raw.strip()
        return [cleaned] if cleaned else []
    if isinstance(raw, (list, tuple)):
        out: list[str] = []
        for item in raw:
            if isinstance(item, str):
                cleaned = item.strip()
                if cleaned:
                    out.append(cleaned)
            elif item is not None:
                out.append(str(item))
        return out
    return [str(raw)]


def _parse_dt(raw: Any) -> datetime | None:
    if raw is None:
        return None
    if isinstance(raw, datetime):
        return raw if raw.tzinfo else raw.replace(tzinfo=timezone.utc)
    if isinstance(raw, str):
        text = raw.strip()
        if not text:
            return None
        # Tolerate trailing 'Z' for ISO8601 UTC.
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        try:
            dt = datetime.fromisoformat(text)
        except ValueError:
            return None
        return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)
    return None


def _json_safe(value: Any) -> Any:
    """Best-effort coercion of frontmatter values to JSON-safe types.

    ``python-frontmatter`` yields native Python objects (datetime, date),
    which the gRPC ``Struct`` translation can't serialize directly.
    """
    if isinstance(value, dict):
        return {str(k): _json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_safe(v) for v in value]
    if isinstance(value, datetime):
        return value.isoformat()
    # `datetime.date` (yaml's default for bare YYYY-MM-DD) — covered by
    # importing only at use time to avoid extra imports up top.
    try:
        from datetime import date as _date

        if isinstance(value, _date):
            return value.isoformat()
    except Exception:  # pragma: no cover
        pass
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return str(value)
