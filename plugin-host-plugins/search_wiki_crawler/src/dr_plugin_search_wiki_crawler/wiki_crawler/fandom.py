"""Warhammer Fantasy (Fandom) MediaWiki API crawler.

Unlike Lexicanum, Fandom doesn't gate the api.php endpoint, so we use
``action=parse`` to get rendered HTML + redirects + categories in one
call. Supports category enumeration, explicit seed lists, and BFS from
a default seed.
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections import deque
from typing import AsyncIterator
from urllib.parse import urljoin

import httpx
from bs4 import BeautifulSoup
from markdownify import markdownify as _markdownify

from .common import WikiPage, _USER_AGENT, _collapse_blank_lines

logger = logging.getLogger(__name__)


_FAN_BASE = "https://warhammerfantasy.fandom.com"
_FAN_DELAY_S = 1.0  # Fandom API is happy with 1 req/sec


# Curated WHFB seed set — anchors the crawler when no category list is
# supplied. The Fandom API does support categorymembers, so most users
# will pass categories instead; these seeds are a fallback / smoke test.
WHFB_FANDOM_SEEDS: tuple[str, ...] = (
    "Sigmar", "Karl_Franz", "Magnus_the_Pious", "Nagash", "Settra",
    "Archaon", "Gotrek_Gurnisson", "Felix_Jaeger",
    "Empire", "Bretonnia", "Dwarfs", "High_Elves", "Dark_Elves",
    "Wood_Elves", "Lizardmen", "Skaven", "Orcs_and_Goblins",
    "Vampire_Counts", "Tomb_Kings", "Chaos_Warriors", "Beastmen",
    "Ogre_Kingdoms", "Daemons_of_Chaos",
    "Khorne", "Slaanesh", "Tzeentch", "Nurgle",
    "Old_World", "Reikland", "Altdorf", "Marienburg", "Ulthuan",
    "Naggaroth", "Lustria", "Athel_Loren", "Karaz-a-Karak",
    "Storm_of_Chaos", "End_Times",
)


class FandomCrawler:
    """MediaWiki API crawler for a Fandom wiki (default: Warhammer Fantasy)."""

    source = "warhammerfantasy_fandom"
    base_url = _FAN_BASE
    crawl_delay_s = _FAN_DELAY_S

    def __init__(
        self,
        *,
        base_url: str | None = None,
        seeds: tuple[str, ...] | None = None,
        max_pages: int = 5_000,
        client: httpx.AsyncClient | None = None,
        user_agent: str | None = None,
    ) -> None:
        self.base_url = (base_url or _FAN_BASE).rstrip("/")
        self._seeds = seeds or WHFB_FANDOM_SEEDS
        self._max_pages = int(max_pages)
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(
            headers={"User-Agent": user_agent or _USER_AGENT, "Accept": "application/json"},
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

    async def _api(self, params: dict) -> dict | None:
        await self._polite()
        url = urljoin(self.base_url + "/", "api.php")
        try:
            response = await self._client.get(url, params=params)
            response.raise_for_status()
            data = response.json()
            if isinstance(data, dict):
                return data
        except Exception as exc:  # noqa: BLE001
            logger.warning("fandom: api %s failed: %s", params.get("action"), exc)
        return None

    async def _enumerate_category(
        self, category: str, *, limit: int
    ) -> AsyncIterator[str]:
        cmcontinue: str | None = None
        yielded = 0
        while yielded < limit:
            params = {
                "action": "query",
                "list": "categorymembers",
                "cmtitle": category if category.startswith("Category:") else f"Category:{category}",
                "cmlimit": str(min(500, limit - yielded)),
                "cmtype": "page",
                "format": "json",
            }
            if cmcontinue:
                params["cmcontinue"] = cmcontinue
            data = await self._api(params)
            if not data:
                return
            for entry in (data.get("query") or {}).get("categorymembers") or []:
                title = entry.get("title")
                if title:
                    yield title.replace(" ", "_")
                    yielded += 1
                    if yielded >= limit:
                        return
            cont = data.get("continue") or {}
            cmcontinue = cont.get("cmcontinue")
            if not cmcontinue:
                return

    async def fetch_page(self, title: str) -> WikiPage | None:
        """Public single-page fetcher; also reused by ``crawl()``.

        ``parse`` returns rendered HTML + redirects + categories in one call.
        """
        slug = title.replace(" ", "_")
        params = {
            "action": "parse",
            "page": slug,
            "prop": "text|categories|links|displaytitle",
            "redirects": "true",
            "format": "json",
            "formatversion": "2",
        }
        data = await self._api(params)
        if not data or "parse" not in data:
            return None
        parse = data["parse"]
        html = (parse.get("text") or "")
        if not html:
            return None

        soup = BeautifulSoup(html, "lxml")
        for selector in [
            ".mw-editsection", ".reference", ".references", ".navbox",
            ".thumb", "table.infobox", ".mw-empty-elt", "script", "style",
        ]:
            for tag in soup.select(selector):
                tag.decompose()

        body = _markdownify(str(soup), heading_style="ATX")
        body = _collapse_blank_lines(body).strip()
        if not body:
            return None

        rendered_title = parse.get("displaytitle") or parse.get("title") or title
        # Strip HTML tags from displaytitle (sometimes contains <i>).
        rendered_title = BeautifulSoup(str(rendered_title), "lxml").get_text().strip()

        categories = [
            c.get("category", "").replace("_", " ").strip()
            for c in (parse.get("categories") or [])
            if isinstance(c, dict)
        ]
        categories = [c for c in categories if c]

        # Internal links — for BFS expansion. Main namespace only (ns=0).
        internal_links: list[str] = []
        for link in (parse.get("links") or []):
            if not isinstance(link, dict):
                continue
            if link.get("ns") != 0:
                continue
            t = link.get("title")
            if t:
                internal_links.append(t.replace(" ", "_"))

        # Aliases from redirect chain.
        aliases: list[str] = []
        for r in (parse.get("redirects") or []):
            if isinstance(r, dict) and r.get("from"):
                aliases.append(str(r["from"]))

        return WikiPage(
            source=self.source,
            title=rendered_title,
            url=urljoin(self.base_url + "/", f"wiki/{slug}"),
            body_markdown=body,
            aliases=aliases,
            categories=categories,
            internal_links=internal_links,
            metadata={"slug": slug},
        )

    async def crawl(
        self,
        *,
        category: str | None = None,
        seeds: tuple[str, ...] | None = None,
        max_pages: int | None = None,
    ) -> AsyncIterator[WikiPage]:
        """Crawl by category, by explicit seeds, or by BFS from defaults."""

        cap = int(max_pages or self._max_pages)
        seen: set[str] = set()

        if category:
            async for title in self._enumerate_category(category, limit=cap):
                if title in seen:
                    continue
                seen.add(title)
                page = await self.fetch_page(title)
                if page is not None:
                    yield page
            return

        queue: deque[str] = deque(seeds or self._seeds)
        while queue and len(seen) < cap:
            title = queue.popleft()
            if title in seen:
                continue
            seen.add(title)
            page = await self.fetch_page(title)
            if page is None:
                continue
            yield page
            for link in page.internal_links:
                if link not in seen and len(queue) + len(seen) < cap * 4:
                    queue.append(link)
