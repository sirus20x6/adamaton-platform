"""pkg.go.dev HTML-scrape adapter.

pkg.go.dev does not expose a JSON search API; this adapter fetches the
public search results page and pulls out the package list with selectolax.
The HTML layout occasionally changes, so the parser is intentionally
defensive: anything we can't parse becomes an empty result list rather
than a hard failure.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any
from urllib.parse import quote_plus

import httpx
from selectolax.parser import HTMLParser

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_BASE_URL = "https://pkg.go.dev"
_USER_AGENT = (
    "adamaton-deepresearch/search_pkggo "
    "(+https://github.com/sirus20x6)"
)


class PkgGoDevAdapter:
    name = "pkggo"
    source_kind = SourceKind.REPO

    def __init__(self) -> None:
        self._headers = {
            "User-Agent": _USER_AGENT,
            "Accept": "text/html,application/xhtml+xml",
        }

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,  # noqa: ARG002 — not supported upstream
    ) -> SearchPage:
        page_num = 1
        if cursor:
            try:
                page_num = max(int(cursor), 1)
            except ValueError:
                page_num = 1

        url = f"{_BASE_URL}/search?q={quote_plus(query)}&page={page_num}"

        async with httpx.AsyncClient(
            timeout=30.0,
            follow_redirects=True,
            headers=self._headers,
        ) as client:
            resp = await client.get(url)
            resp.raise_for_status()
            html = resp.text

        results = _parse_results(html, limit=limit)

        # pkg.go.dev paginates; if we filled the limit, assume more pages exist.
        # The HTML's pagination link is unreliable across layouts, so the
        # cursor advances whenever the page returned at least `limit` rows.
        next_cursor = str(page_num + 1) if len(results) >= limit and results else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=0,
        )


def _parse_results(html: str, *, limit: int) -> list[SearchResult]:
    """Best-effort parse of the pkg.go.dev search results page.

    Returns an empty list if the layout has shifted enough that we can't
    find any snippets.
    """
    try:
        tree = HTMLParser(html)
    except Exception:
        return []

    out: list[SearchResult] = []
    try:
        snippets = tree.css("div.SearchSnippet")
        for node in snippets:
            if len(out) >= limit:
                break
            hit = _parse_snippet(node)
            if hit is not None:
                out.append(hit)
    except Exception:
        return out
    return out


def _parse_snippet(node: Any) -> SearchResult | None:
    """Parse a single SearchSnippet div. Returns None on any failure."""
    try:
        # The header anchor carries the import path in its href and text.
        anchor = None
        for sel in (
            "h2.SearchSnippet-headerContainer a",
            "h2.SearchSnippet-header a",
            "div.SearchSnippet-headerContainer a",
            "h2 a",
        ):
            anchor = node.css_first(sel)
            if anchor is not None:
                break
        if anchor is None:
            return None

        href = (anchor.attributes.get("href") or "").strip()
        if not href:
            return None
        # href is typically "/k8s.io/client-go" — strip the leading slash.
        import_path = href.lstrip("/")
        # Strip a possible query string / fragment.
        for sep in ("?", "#"):
            if sep in import_path:
                import_path = import_path.split(sep, 1)[0]
        if not import_path:
            return None

        title_text = (anchor.text(strip=True) or import_path).strip()

        synopsis = ""
        for sel in (
            "p.SearchSnippet-synopsis",
            "div.SearchSnippet-synopsis",
            "p.go-textSubtle",
        ):
            syn_node = node.css_first(sel)
            if syn_node is not None:
                synopsis = syn_node.text(strip=True) or ""
                if synopsis:
                    break

        raw: dict[str, Any] = {"import_path": import_path}

        # Best-effort badge / metadata extraction. pkg.go.dev surfaces
        # imported-by counts and other infra in spans/divs that change
        # names; we capture whatever text we can find without insisting.
        try:
            infos = node.css("div.SearchSnippet-infoLabel, span.SearchSnippet-infoLabel")
            badges: list[str] = []
            for info in infos:
                text = info.text(strip=True)
                if text:
                    badges.append(text)
            if badges:
                raw["info_labels"] = badges
        except Exception:
            pass

        return SearchResult(
            adapter="pkggo",
            external_id=import_path,
            title=title_text or import_path,
            url=f"{_BASE_URL}/{import_path}",
            abstract=synopsis,
            authors=[],
            published_at=None,
            venue="pkg.go.dev",
            citation_count=0,
            raw=raw,
            score=0.0,
            source_kind=SourceKind.REPO,
        )
    except Exception:
        return None
