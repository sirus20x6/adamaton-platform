"""Shared dataclass + helpers across all three crawlers."""

from __future__ import annotations

from dataclasses import dataclass, field


_USER_AGENT = (
    "deepresearch-platform/0.1 (https://github.com/sirus20x6/deepresearch; sirus20x6@gmail.com)"
)


@dataclass(slots=True)
class WikiPage:
    """One crawled article ready for ingest."""

    source: str  # 'lexicanum' | 'tvtropes' | 'warhammerfantasy_fandom'
    title: str
    url: str
    body_markdown: str
    aliases: list[str] = field(default_factory=list)
    categories: list[str] = field(default_factory=list)
    internal_links: list[str] = field(default_factory=list)
    metadata: dict = field(default_factory=dict)


def _collapse_blank_lines(value: str) -> str:
    # Two consecutive blank lines is enough for paragraph breaks; more than
    # that bloats the markdown without helping the reader.
    lines = [line.rstrip() for line in value.splitlines()]
    out: list[str] = []
    blank = 0
    for line in lines:
        if line.strip():
            out.append(line)
            blank = 0
        else:
            blank += 1
            if blank <= 1:
                out.append("")
    return "\n".join(out)
