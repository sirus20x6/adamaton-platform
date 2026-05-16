"""Lexicanum (Warhammer 40k wiki) BFS HTML crawler.

Why HTML and not the MediaWiki API: Lexicanum's api.php is behind
Cloudflare's bot challenge (HTTP 403 even with browser UAs); the
``/wiki/<Title>`` pages serve normally. We BFS from a seed list of
article titles, follow internal ``/wiki/...`` hrefs, and deduplicate
by slug.
"""

from __future__ import annotations

import asyncio
import logging
import re
import time
from collections import deque
from typing import AsyncIterator
from urllib.parse import unquote, urljoin

import httpx
from bs4 import BeautifulSoup
from markdownify import markdownify as _markdownify

from .common import WikiPage, _USER_AGENT, _collapse_blank_lines

logger = logging.getLogger(__name__)


_LEX_BASE = "https://wh40k.lexicanum.com"
_LEX_DELAY_S = 5.0  # robots.txt Crawl-delay


# Curated W40k seed set — most-referenced articles. The crawler BFS-
# expands from here, so the reachable subset of Lexicanum is fully
# covered without needing the (Cloudflare-blocked) category index.
LEXICANUM_SEEDS_W40K: tuple[str, ...] = (
    # Primarchs + Emperor
    "Horus_Lupercal", "Roboute_Guilliman", "Magnus_the_Red", "Leman_Russ",
    "Lion_El'Jonson", "Sanguinius", "Ferrus_Manus", "Vulkan",
    "Konrad_Curze", "Mortarion", "Fulgrim", "Perturabo", "Angron",
    "Lorgar", "Alpharius_Omegon", "Corvus_Corax", "Jaghatai_Khan",
    "Rogal_Dorn", "Emperor_of_Mankind",
    # Major factions
    "Imperium", "Adeptus_Astartes", "Imperial_Guard", "Adeptus_Mechanicus",
    "Inquisition", "Adepta_Sororitas", "Officio_Assassinorum",
    "Chaos", "Chaos_Space_Marines", "Black_Legion", "World_Eaters",
    "Death_Guard", "Thousand_Sons", "Word_Bearers", "Emperor's_Children",
    "Night_Lords", "Iron_Warriors",
    # Xenos
    "Aeldari", "Drukhari", "Necrons", "Tyranids", "Orks", "T'au",
    "Genestealer_Cults", "Leagues_of_Votann",
    # Chaos Gods + daemons
    "Khorne", "Slaanesh", "Tzeentch", "Nurgle", "Daemon",
    # Settings
    "Terra", "Mars", "Cadia", "Macragge", "Fenris", "Prospero", "Caliban",
    "Eye_of_Terror", "Warp", "Webway",
    # Events / eras
    "Horus_Heresy", "Great_Crusade", "Battle_of_Terra", "Fall_of_Cadia",
    "M30", "M31", "M41", "M42",
    # Tech / artefacts
    "Bolter", "Power_Armour", "Land_Raider", "Titan", "Imperial_Knight",
)


_INTERNAL_HREF_RE = re.compile(r'^/wiki/([^"#:]+)$')


class LexicanumCrawler:
    """BFS HTML crawler for Lexicanum (Warhammer 40k wiki)."""

    source = "lexicanum"
    base_url = _LEX_BASE
    crawl_delay_s = _LEX_DELAY_S

    def __init__(
        self,
        *,
        seeds: tuple[str, ...] | None = None,
        max_pages: int = 5_000,
        client: httpx.AsyncClient | None = None,
        user_agent: str | None = None,
    ) -> None:
        self._seeds = seeds or LEXICANUM_SEEDS_W40K
        self._max_pages = int(max_pages)
        self._owns_client = client is None
        # HTTP/1.1 only — Cloudflare's bot challenge fires on httpx's h2
        # client fingerprint. Curl over h1 sails through with the same UA.
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
            response.raise_for_status()
            return response.text
        except Exception as exc:  # noqa: BLE001
            logger.warning("lexicanum: fetch %s failed: %s", path, exc)
            return None

    def _parse(self, html: str, *, title: str, url: str) -> WikiPage | None:
        soup = BeautifulSoup(html, "lxml")
        content = soup.find("div", id="mw-content-text")
        if content is None:
            return None

        # Strip nav / edit chrome before markdown conversion.
        for selector in [
            ".mw-editsection",
            ".navbox",
            ".thumb",
            ".printfooter",
            ".catlinks",
            ".reference",
            ".references",
            "#toc",
        ]:
            for tag in content.select(selector):
                tag.decompose()

        # Internal links (for BFS expansion + as KG seed signal).
        internal_links: list[str] = []
        for a in content.find_all("a", href=True):
            m = _INTERNAL_HREF_RE.match(a["href"])
            if m is None:
                continue
            target = unquote(m.group(1)).split("#")[0]
            if not target or target in internal_links:
                continue
            internal_links.append(target)

        body = _markdownify(str(content), heading_style="ATX")
        body = _collapse_blank_lines(body).strip()

        # Categories (rendered HTML keeps them in ``catlinks`` even when
        # we've stripped that selector above — re-find on the original soup).
        categories: list[str] = []
        catlinks = soup.find("div", id="catlinks")
        if catlinks:
            for a in catlinks.find_all("a"):
                cat = (a.get_text() or "").strip()
                if cat and cat not in categories:
                    categories.append(cat)

        # Title from <h1> when available; falls back to the URL slug.
        h1 = soup.find("h1", id="firstHeading")
        rendered_title = (h1.get_text().strip() if h1 else title).strip()

        if not body:
            return None

        return WikiPage(
            source=self.source,
            title=rendered_title,
            url=url,
            body_markdown=body,
            categories=categories,
            internal_links=internal_links,
            metadata={"slug": title},
        )

    async def crawl(
        self,
        *,
        max_pages: int | None = None,
        seeds: tuple[str, ...] | None = None,
    ) -> AsyncIterator[WikiPage]:
        """BFS crawl yielding one :class:`WikiPage` per visited article."""

        seen: set[str] = set()
        queue: deque[str] = deque(seeds or self._seeds)
        cap = int(max_pages or self._max_pages)
        while queue and len(seen) < cap:
            slug = queue.popleft()
            if slug in seen:
                continue
            seen.add(slug)
            html = await self._fetch(f"/wiki/{slug}")
            if html is None:
                continue
            page = self._parse(html, title=slug, url=urljoin(self.base_url, f"/wiki/{slug}"))
            if page is None:
                continue
            yield page
            # Enqueue internal links the page surfaced. cap*4 keeps the
            # queue bounded so a runaway crawl can't blow the lo queue.
            for link in page.internal_links:
                if link not in seen and len(queue) + len(seen) < cap * 4:
                    queue.append(link)
