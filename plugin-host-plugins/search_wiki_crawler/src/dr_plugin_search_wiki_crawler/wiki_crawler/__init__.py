"""Wiki crawlers ported verbatim from app/adapters/wiki_crawler.py.

Split into one module per back-end so the file stays browsable, but
behavior is identical to the legacy single-file version — same seed
sets, same selectors, same politeness budgets.
"""

from __future__ import annotations

from .common import WikiPage, _collapse_blank_lines, _USER_AGENT
from .fandom import FandomCrawler, WHFB_FANDOM_SEEDS
from .lexicanum import LEXICANUM_SEEDS_W40K, LexicanumCrawler
from .tvtropes import TVTropesCrawler

__all__ = [
    "FandomCrawler",
    "LEXICANUM_SEEDS_W40K",
    "LexicanumCrawler",
    "TVTropesCrawler",
    "WHFB_FANDOM_SEEDS",
    "WikiPage",
]
