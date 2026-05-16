"""TVTropes BFS HTML crawler for the Warhammer40000 namespace.

Starts from ``/pmwiki/pmwiki.php/TabletopGame/Warhammer40000`` (the W40k
index page) and follows internal trope links. Bounded by ``max_pages``
so we don't accidentally crawl the whole site.
"""

from __future__ import annotations

import asyncio
import logging
import re
import time
from collections import deque
from typing import AsyncIterator
from urllib.parse import urljoin, urlparse

import httpx
from bs4 import BeautifulSoup
from markdownify import markdownify as _markdownify

from .common import WikiPage, _USER_AGENT, _collapse_blank_lines

logger = logging.getLogger(__name__)


_TVT_BASE = "https://tvtropes.org"
_TVT_DELAY_S = 2.0


class TVTropesCrawler:
    """BFS crawler for TVTropes' Warhammer40000 namespace."""

    source = "tvtropes"
    base_url = _TVT_BASE
    crawl_delay_s = _TVT_DELAY_S

    def __init__(
        self,
        *,
        # TVTropes moved Warhammer40000 out of the /Main/ namespace into
        # /TabletopGame/. The old path renders as an empty stub (1KB
        # placeholder); the new one is the real ~54KB article.
        seed_path: str = "/pmwiki/pmwiki.php/TabletopGame/Warhammer40000",
        max_pages: int = 2_000,
        client: httpx.AsyncClient | None = None,
        user_agent: str | None = None,
    ) -> None:
        self._seed_path = seed_path
        self._max_pages = int(max_pages)
        self._owns_client = client is None
        # HTTP/1.1 to dodge bot-fingerprint blocks on h2.
        self._client = client or httpx.AsyncClient(
            headers={"User-Agent": user_agent or _USER_AGENT, "Accept": "text/html"},
            timeout=httpx.Timeout(30.0, connect=10.0),
            follow_redirects=True,
            http2=False,
        )
        self._last_request_at: float = 0.0

    async def aclose(self) -> None:
        if self._owns_client and not self._client.is_closed:
            await self._client.aclose()

    async def _polite(self) -> None:
        elapsed = time.monotonic() - self._last_request_at
        wait = self.crawl_delay_s - elapsed
        if wait > 0:
            await asyncio.sleep(wait)
        self._last_request_at = time.monotonic()

    async def _fetch(self, path: str) -> str | None:
        await self._polite()
        url = urljoin(self.base_url, path)
        try:
            response = await self._client.get(url)
            # TVTropes serves real content with HTTP 404 for /Main/ pages
            # by convention. Treat 404 as success when the body looks
            # like a valid wiki page.
            if response.status_code in (200, 404):
                if "wikitext" in response.text or "wiki-content" in response.text:
                    return response.text
                return response.text  # fallthrough: parser decides
            response.raise_for_status()
            return response.text
        except Exception as exc:  # noqa: BLE001
            logger.warning("tvtropes: fetch %s failed: %s", path, exc)
            return None

    @staticmethod
    def _path_to_title(path: str) -> str:
        last = path.rstrip("/").split("/")[-1]
        # Convert WikiCase to spaced words.
        return re.sub(r"(?<!^)(?=[A-Z])", " ", last).strip()

    def _parse(self, html: str, *, path: str) -> WikiPage | None:
        soup = BeautifulSoup(html, "lxml")
        # TVTropes wraps the body in ``<div id="main-article"
        # class="article-content">`` inside ``<article id="main-entry">``.
        # Earlier versions of this code looked for ``<article id="main-article">``
        # which doesn't exist on the current site — every fetch silently
        # returned None and the crawl produced 0 docs.
        article = (
            soup.find("div", id="main-article")
            or soup.find("article", id="main-entry")
            or soup.find("div", id="wikitext")
            or soup.find("div", class_="page-content")
        )
        if article is None:
            return None

        # Strip TVTropes chrome.
        for selector in [
            ".header-inner-wrapper",
            ".action-bar",
            ".search-bar",
            "#wikitext .pagetitle",
            "script",
            "style",
            ".ad-container",
            ".trope-bar",
        ]:
            for tag in article.select(selector):
                tag.decompose()

        # BFS targets: internal trope + franchise links across all the
        # namespaces a Warhammer crawl might touch.
        allowed_namespaces = (
            "/pmwiki/pmwiki.php/Main/",
            "/pmwiki/pmwiki.php/TabletopGame/",
            "/pmwiki/pmwiki.php/Franchise/",
            "/pmwiki/pmwiki.php/Characters/",
            "/pmwiki/pmwiki.php/UsefulNotes/",
        )
        internal_links: list[str] = []
        for a in article.find_all("a", href=True):
            href = a["href"]
            parsed = urlparse(href)
            if parsed.netloc and parsed.netloc != "tvtropes.org":
                continue
            path_only = parsed.path or href
            if not path_only.startswith(allowed_namespaces):
                continue
            if path_only == path:
                continue
            if path_only not in internal_links:
                internal_links.append(path_only)

        body = _markdownify(str(article), heading_style="ATX")
        body = _collapse_blank_lines(body).strip()
        if not body:
            return None

        title = self._path_to_title(path)
        return WikiPage(
            source=self.source,
            title=title or "(untitled)",
            url=urljoin(self.base_url, path),
            body_markdown=body,
            categories=[],
            internal_links=internal_links,
            metadata={"path": path},
        )

    async def crawl(
        self,
        *,
        max_pages: int | None = None,
        seed_path: str | None = None,
    ) -> AsyncIterator[WikiPage]:
        seen: set[str] = set()
        queue: deque[str] = deque([seed_path or self._seed_path])
        cap = int(max_pages or self._max_pages)
        while queue and len(seen) < cap:
            path = queue.popleft()
            if path in seen:
                continue
            seen.add(path)
            html = await self._fetch(path)
            if html is None:
                continue
            page = self._parse(html, path=path)
            if page is None:
                continue
            yield page
            for link in page.internal_links:
                if link not in seen and len(queue) + len(seen) < cap * 4:
                    queue.append(link)
