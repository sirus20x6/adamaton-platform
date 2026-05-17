"""PyPI adapter.

PyPI deprecated its XMLRPC ``search`` endpoint in 2021 and never replaced
it with a JSON search API. This adapter therefore **scrapes** the public
HTML search page at ``https://pypi.org/search/?q={q}&page=N`` using httpx
+ selectolax for ranked package hits, then optionally enriches the top
few results via the per-package JSON endpoint
``https://pypi.org/pypi/{name}/json``.

HTML selectors used:
- ``.package-snippet__name``        -> package name (external_id, title)
- ``.package-snippet__description`` -> short abstract
- ``.package-snippet__version``     -> latest version string (stashed in raw)
- ``.package-snippet__released``    -> ``<time datetime="...">`` for the
                                      release date

Pagination: the HTML listing is 20 results per page. ``cursor`` is the
1-indexed next page number as a decimal string; empty cursor means page 1.

Date filtering: PyPI's search page exposes no ``since`` filter, so we
apply ``since`` client-side against the parsed release date from the
``<time>`` element (or the JSON metadata when we hit it).
"""

from __future__ import annotations

import asyncio
from datetime import datetime, timezone
from typing import Any

import httpx
from selectolax.parser import HTMLParser, Node

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


PYPI_SEARCH_URL = "https://pypi.org/search/"
PYPI_JSON_URL = "https://pypi.org/pypi/{name}/json"
USER_AGENT = (
    "adamaton-deepresearch/search_pypi "
    "(+https://github.com/sirus20x6; mailto:sirus20x6@gmail.com)"
)

# How many of the top results we enrich via the per-package JSON endpoint
# per page. Keeps the plugin fast while still surfacing richer metadata
# (author, project URLs, classifiers, license) for the highest-ranked
# hits the user is most likely to care about.
JSON_ENRICH_LIMIT = 5
# Results per HTML page on pypi.org/search.
RESULTS_PER_PAGE = 20


class PyPIAdapter:
    name = "pypi"
    source_kind = SourceKind.REPO

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={
                "User-Agent": USER_AGENT,
                "Accept": "text/html,application/xhtml+xml,application/json",
            },
            follow_redirects=True,
        )

    @staticmethod
    def _text(node: Node | None) -> str:
        if node is None:
            return ""
        return (node.text() or "").strip()

    @staticmethod
    def _parse_iso(value: str | None) -> datetime | None:
        if not value:
            return None
        # pypi.org emits attribute values like "2024-01-15T12:34:56+0000".
        # datetime.fromisoformat accepts ±HH:MM but not ±HHMM in Python <3.11,
        # so normalize the trailing offset before parsing.
        v = value.strip()
        if v.endswith("Z"):
            v = v[:-1] + "+00:00"
        elif len(v) >= 5 and (v[-5] in "+-") and v[-3] != ":":
            v = v[:-2] + ":" + v[-2:]
        try:
            dt = datetime.fromisoformat(v)
        except ValueError:
            return None
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt

    def _parse_snippet(self, snippet: Node) -> dict[str, Any] | None:
        name_node = snippet.css_first(".package-snippet__name")
        name = self._text(name_node)
        if not name:
            return None

        desc = self._text(snippet.css_first(".package-snippet__description"))
        version = self._text(snippet.css_first(".package-snippet__version"))

        released_node = snippet.css_first(".package-snippet__released time")
        released_attr = released_node.attributes.get("datetime") if released_node else None
        released_at = self._parse_iso(released_attr)

        return {
            "name": name,
            "description": desc,
            "version": version,
            "released_at": released_at,
            "released_raw": released_attr or "",
        }

    async def _fetch_json(self, name: str) -> dict[str, Any] | None:
        try:
            resp = await self._client.get(PYPI_JSON_URL.format(name=name))
        except httpx.HTTPError:
            return None
        if resp.status_code != 200:
            return None
        try:
            return resp.json()
        except ValueError:
            return None

    @staticmethod
    def _authors_from_info(info: dict[str, Any]) -> list[str]:
        authors: list[str] = []
        for key in ("author", "maintainer"):
            raw = info.get(key)
            if isinstance(raw, str) and raw.strip():
                # PyPI commonly comma-separates multi-author strings.
                for piece in raw.split(","):
                    p = piece.strip()
                    if p and p not in authors:
                        authors.append(p)
        return authors

    def _hit_from_snippet(self, snippet_data: dict[str, Any]) -> SearchResult:
        name = snippet_data["name"]
        raw: dict[str, Any] = {
            "name": name,
            "version": snippet_data.get("version") or "",
            "released_raw": snippet_data.get("released_raw") or "",
            "source": "pypi.org/search",
        }
        return SearchResult(
            adapter=self.name,
            external_id=name,
            title=name,
            url=f"https://pypi.org/project/{name}/",
            abstract=snippet_data.get("description") or "",
            authors=[],
            published_at=snippet_data.get("released_at"),
            venue="PyPI",
            citation_count=0,
            raw=raw,
            score=0.0,
            source_kind=SourceKind.REPO,
        )

    def _merge_json(
        self,
        hit: SearchResult,
        payload: dict[str, Any],
    ) -> SearchResult:
        info = payload.get("info") or {}
        if not isinstance(info, dict):
            return hit

        summary = (info.get("summary") or "").strip()
        if summary and not hit.abstract:
            hit.abstract = summary

        authors = self._authors_from_info(info)
        if authors:
            hit.authors = authors

        # Prefer the project URL from JSON if present.
        project_url = (info.get("project_url") or info.get("package_url") or "").strip()
        if project_url:
            hit.url = project_url

        # Stash the rich-ish metadata; keep the payload bounded by recording
        # only the fields the host is likely to consume.
        hit.raw.update(
            {
                "version": info.get("version") or hit.raw.get("version") or "",
                "summary": summary,
                "home_page": info.get("home_page") or "",
                "project_urls": info.get("project_urls") or {},
                "license": info.get("license") or "",
                "classifiers": info.get("classifiers") or [],
                "requires_python": info.get("requires_python") or "",
                "keywords": info.get("keywords") or "",
                "yanked": info.get("yanked", False),
                "json_enriched": True,
            }
        )

        # If the snippet didn't surface a release date, try to derive one
        # from the per-version release timestamps in the JSON payload.
        if hit.published_at is None:
            version = info.get("version")
            releases = payload.get("releases") or {}
            files = releases.get(version) if isinstance(releases, dict) else None
            if isinstance(files, list):
                stamps = []
                for f in files:
                    if not isinstance(f, dict):
                        continue
                    dt = self._parse_iso(f.get("upload_time_iso_8601") or f.get("upload_time"))
                    if dt is not None:
                        stamps.append(dt)
                if stamps:
                    hit.published_at = max(stamps)

        return hit

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        page_num = 1
        if cursor and cursor.isdigit():
            n = int(cursor)
            if n >= 1:
                page_num = n

        params = {"q": query, "page": str(page_num)}
        resp = await self._client.get(PYPI_SEARCH_URL, params=params)
        resp.raise_for_status()

        tree = HTMLParser(resp.text)
        snippets = tree.css("a.package-snippet")
        parsed: list[dict[str, Any]] = []
        for node in snippets:
            data = self._parse_snippet(node)
            if data is not None:
                parsed.append(data)

        # Apply ``since`` filter client-side — PyPI's search UI doesn't
        # support a date filter. Drop hits whose snippet date is older
        # than ``since``; rows missing a date pass through (JSON enrichment
        # may fill it in below, after which we re-check).
        if since is not None:
            kept: list[dict[str, Any]] = []
            for d in parsed:
                released = d.get("released_at")
                if released is None or released >= since:
                    kept.append(d)
            parsed = kept

        max_results = max(int(limit), 1)
        sliced = parsed[:max_results]
        hits = [self._hit_from_snippet(d) for d in sliced]

        # Optionally enrich the top results in parallel via the JSON API.
        to_enrich = hits[:JSON_ENRICH_LIMIT]
        if to_enrich:
            payloads = await asyncio.gather(
                *(self._fetch_json(h.external_id) for h in to_enrich),
                return_exceptions=True,
            )
            for h, p in zip(to_enrich, payloads):
                if isinstance(p, dict):
                    self._merge_json(h, p)

        # After JSON enrichment, re-apply ``since`` for any rows whose
        # date only became available from the JSON payload.
        if since is not None:
            hits = [
                h for h in hits
                if h.published_at is None or h.published_at >= since
            ]

        # If this page filled the slot, assume there may be another page.
        if len(parsed) >= RESULTS_PER_PAGE:
            next_cursor = str(page_num + 1)
        else:
            next_cursor = ""

        return SearchPage(
            results=hits,
            next_cursor=next_cursor,
            total_estimated=0,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
