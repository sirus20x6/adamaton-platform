"""OECD iLibrary adapter.

OECD does not expose a clean public search API, so this adapter scrapes the
iLibrary search HTML page:

    https://www.oecd-ilibrary.org/search?value1={q}&option1=fulltext
        &pageSize={N}&page={M}

The HTML structure we target uses ``.search-result`` blocks each containing:

* ``.search-result__title a[href]`` — title text + relative href to the work
* ``.search-result__authors``       — comma-separated author names
* ``.search-result__date``          — short date string ("Jul 2024", "2023", ...)
* ``.search-result__summary``       — abstract / summary text

This is BEST-EFFORT: OECD's markup is brittle and undocumented. Every row is
parsed defensively inside a ``try/except`` so a single malformed entry never
sinks the whole page. If iLibrary changes its markup, this adapter will
degrade silently (returning fewer or empty results) rather than crash; that
behaviour is intentional. A failing per-row parse is captured into
``raw["_parse_error"]`` for downstream debugging.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any
from urllib.parse import urljoin

import httpx
from selectolax.parser import HTMLParser, Node

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


USER_AGENT = (
    "adamaton-deepresearch/search_oecd (+https://github.com/sirus20x6)"
)
BASE_URL = "https://www.oecd-ilibrary.org"
SEARCH_URL = f"{BASE_URL}/search"


# Month names we tolerate when an iLibrary date is given as "Jul 2024",
# "July 2024", "07 Jul 2024", etc. We deliberately keep this small and forgiving.
_MONTHS = {
    "jan": 1, "january": 1,
    "feb": 2, "february": 2,
    "mar": 3, "march": 3,
    "apr": 4, "april": 4,
    "may": 5,
    "jun": 6, "june": 6,
    "jul": 7, "july": 7,
    "aug": 8, "august": 8,
    "sep": 9, "sept": 9, "september": 9,
    "oct": 10, "october": 10,
    "nov": 11, "november": 11,
    "dec": 12, "december": 12,
}


def _clean(text: str | None) -> str:
    if not text:
        return ""
    return " ".join(text.split()).strip()


def _node_text(node: Node | None) -> str:
    if node is None:
        return ""
    try:
        return _clean(node.text(separator=" ", strip=True))
    except Exception:
        return ""


def _parse_date(s: str) -> datetime | None:
    """Parse OECD date strings defensively.

    Accepts forms like ``2024``, ``Jul 2024``, ``July 2024``, ``07 Jul 2024``,
    ``2024-07-15``, ``2024/07/15``. Returns None on anything we don't recognise
    rather than raising — this is an HTML scrape, not a structured feed.
    """
    if not s:
        return None
    text = _clean(s)
    if not text:
        return None

    # ISO-ish first.
    for fmt in ("%Y-%m-%d", "%Y/%m/%d", "%Y-%m", "%Y/%m"):
        try:
            return datetime.strptime(text, fmt)
        except ValueError:
            pass

    tokens = [t.strip(",") for t in text.replace("/", " ").replace("-", " ").split()]
    year: int | None = None
    month: int | None = None
    day: int | None = None
    for tok in tokens:
        low = tok.lower()
        if low in _MONTHS:
            month = _MONTHS[low]
            continue
        if tok.isdigit():
            n = int(tok)
            if n >= 1900 and year is None:
                year = n
            elif 1 <= n <= 31 and day is None:
                day = n
    if year is None:
        return None
    try:
        return datetime(year, month or 1, day or 1)
    except ValueError:
        try:
            return datetime(year, 1, 1)
        except ValueError:
            return None


def _split_authors(raw: str) -> list[str]:
    if not raw:
        return []
    # iLibrary author lines can be comma-separated or include "and" / "&".
    for sep in (";", " and ", " & "):
        raw = raw.replace(sep, ",")
    parts = [p.strip() for p in raw.split(",")]
    return [p for p in parts if p]


def _derive_external_id(url: str) -> str:
    """Pick a stable id from the work URL.

    Prefers a DOI-shaped tail (``10.xxxx/...``); otherwise falls back to the
    last non-empty path segment. Never raises; returns ``oecd:unknown`` if
    the URL is unusable.
    """
    if not url:
        return "oecd:unknown"
    try:
        # OECD DOIs usually appear inline as ``/doi/<doi>`` in the path.
        marker = "/doi/"
        idx = url.find(marker)
        if idx != -1:
            doi = url[idx + len(marker):].split("?", 1)[0].split("#", 1)[0]
            doi = doi.strip("/")
            if doi:
                return f"doi:{doi}"
        tail = url.split("?", 1)[0].split("#", 1)[0].rstrip("/").rsplit("/", 1)[-1]
        return tail or "oecd:unknown"
    except Exception:
        return "oecd:unknown"


class OECDAdapter:
    name = "search_oecd"
    source_kind = SourceKind.WEB

    def __init__(self) -> None:
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={
                "User-Agent": USER_AGENT,
                "Accept": "text/html,application/xhtml+xml",
                "Accept-Language": "en",
            },
            follow_redirects=True,
        )

    def _parse_row(self, row: Node) -> SearchResult | None:
        """Parse a single ``.search-result`` block. Returns None if unusable."""
        try:
            title_link = row.css_first(".search-result__title a")
            href = ""
            title = ""
            if title_link is not None:
                href = (title_link.attributes.get("href") or "").strip()
                title = _node_text(title_link)

            url = urljoin(BASE_URL, href) if href else ""
            external_id = _derive_external_id(url) if url else "oecd:unknown"

            authors_node = row.css_first(".search-result__authors")
            authors = _split_authors(_node_text(authors_node))

            date_node = row.css_first(".search-result__date")
            date_text = _node_text(date_node)
            published_at = _parse_date(date_text)

            summary_node = row.css_first(".search-result__summary")
            abstract = _node_text(summary_node)

            # If we can't get anything at all, skip the row.
            if not (title or url or abstract):
                return None

            raw: dict[str, Any] = {
                "href": href,
                "date_text": date_text,
            }

            return SearchResult(
                adapter=self.name,
                external_id=external_id,
                title=title,
                url=url,
                abstract=abstract,
                authors=authors,
                published_at=published_at,
                venue="OECD iLibrary",
                citation_count=0,
                raw=raw,
                score=0.0,
                source_kind=SourceKind.WEB,
            )
        except Exception as e:  # pragma: no cover - defensive
            try:
                return SearchResult(
                    adapter=self.name,
                    external_id="oecd:parse-error",
                    title="",
                    url="",
                    abstract="",
                    authors=[],
                    published_at=None,
                    venue="OECD iLibrary",
                    citation_count=0,
                    raw={"_parse_error": f"{type(e).__name__}: {e}"},
                    score=0.0,
                    source_kind=SourceKind.WEB,
                )
            except Exception:
                return None

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        page_num = int(cursor) if cursor and cursor.isdigit() and int(cursor) > 0 else 1
        page_size = max(int(limit), 1)

        params = {
            "value1": query,
            "option1": "fulltext",
            "pageSize": str(page_size),
            "value2": "",
            "option2": "",
            "value3": "",
            "option3": "",
            "publicationDate": "",
            "page": str(page_num),
        }

        resp = await self._client.get(SEARCH_URL, params=params)
        resp.raise_for_status()
        html = resp.text

        results: list[SearchResult] = []
        try:
            tree = HTMLParser(html)
            rows = tree.css(".search-result") or []
            for row in rows:
                parsed = self._parse_row(row)
                if parsed is None:
                    continue
                # Drop the synthetic parse-error rows from the user-visible page,
                # but keep going — they were already logged into raw.
                if parsed.external_id == "oecd:parse-error":
                    continue
                results.append(parsed)
        except Exception:
            # Total parse failure — return an empty page rather than 500. The
            # caller will see ``results=[]`` and ``next_cursor=""``.
            return SearchPage(results=[], next_cursor="", total_estimated=0)

        # Client-side date filter — OECD's search doesn't expose a clean
        # since-filter so we apply it after parsing.
        if since is not None:
            results = [
                r for r in results
                if r.published_at is None or r.published_at >= since
            ]

        results = results[:page_size]

        # Best-effort pagination: if we saw a full page, assume there's more.
        next_cursor = str(page_num + 1) if len(results) >= page_size else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=0,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
