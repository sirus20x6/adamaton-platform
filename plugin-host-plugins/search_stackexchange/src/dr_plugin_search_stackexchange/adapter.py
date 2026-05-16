"""Stack Exchange adapter (self-contained copy from legacy
app/adapters/stackexchange.py).

Defaults to stackoverflow.com. ``search()`` calls ``/search/advanced``
with ``filter=withbody``. ``fetch()`` combines the question body with the
top three voted answers (accepted first) into a single markdown document.

Config via env: ``STACKEXCHANGE_KEY`` (api key), ``STACKEXCHANGE_SITE``
(site slug, default ``stackoverflow``).
"""

from __future__ import annotations

import html as html_lib
import os
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Awaitable, Callable, TypeVar

import httpx
from bs4 import BeautifulSoup
from markdownify import markdownify as _markdownify
from tenacity import (
    AsyncRetrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)


# ---- Local protocol mirrors --------------------------------------------


class SourceKind(str, Enum):
    JOURNAL = "journal"
    PREPRINT = "preprint"
    REPO = "repo"
    FORUM = "forum"
    WIKI = "wiki"
    WEB = "web"


@dataclass(kw_only=True, slots=True)
class SearchResult:
    adapter: str
    external_id: str
    title: str
    url: str
    abstract: str | None = None
    authors: list[str] = field(default_factory=list)
    published_at: datetime | None = None
    venue: str | None = None
    citation_count: int | None = None
    raw: dict[str, Any] = field(default_factory=dict)
    score: float | None = None
    source_kind: SourceKind = SourceKind.WEB


@dataclass(kw_only=True, slots=True)
class SearchPage:
    results: list[SearchResult]
    next_cursor: str | None = None
    total_estimated: int | None = None


@dataclass(kw_only=True, slots=True)
class FetchedDoc:
    adapter: str
    external_id: str
    url: str
    title: str
    content_type: str
    body: bytes | str
    source_tier: str
    metadata: dict[str, Any] = field(default_factory=dict)


# ---- HTTP base ----------------------------------------------------------

USER_AGENT = "deepresearch-platform/0.1.0 (+https://github.com/sirus20x6/deepresearch)"

T = TypeVar("T")


def _first(text: str | None) -> str | None:
    if text is None:
        return None
    stripped = text.strip()
    return stripped or None


def _is_transient(exc: BaseException) -> bool:
    if isinstance(exc, httpx.HTTPStatusError):
        return exc.response.status_code in (429, 500, 502, 503, 504)
    return isinstance(
        exc,
        (
            httpx.ConnectError,
            httpx.ReadError,
            httpx.WriteError,
            httpx.RemoteProtocolError,
            httpx.ReadTimeout,
            httpx.ConnectTimeout,
            httpx.PoolTimeout,
        ),
    )


class _BaseHttpAdapter:
    def __init__(
        self,
        *,
        base_url: str | None = None,
        headers: dict[str, str] | None = None,
        timeout: float | httpx.Timeout | None = None,
        http2: bool = True,
    ) -> None:
        merged: dict[str, str] = {
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        }
        if headers:
            merged.update(headers)
        try:
            self._client = httpx.AsyncClient(
                base_url=base_url or "",
                headers=merged,
                timeout=timeout if timeout is not None else httpx.Timeout(15.0, connect=5.0),
                http2=http2,
                follow_redirects=True,
            )
        except ImportError:
            self._client = httpx.AsyncClient(
                base_url=base_url or "",
                headers=merged,
                timeout=timeout if timeout is not None else httpx.Timeout(15.0, connect=5.0),
                http2=False,
                follow_redirects=True,
            )

    async def aclose(self) -> None:
        if not self._client.is_closed:
            await self._client.aclose()

    async def _retry(self, op: Callable[[], Awaitable[T]]) -> T:
        async for attempt in AsyncRetrying(
            stop=stop_after_attempt(3),
            wait=wait_exponential(multiplier=0.5, min=0.5, max=8.0),
            retry=retry_if_exception(_is_transient),
            reraise=True,
        ):
            with attempt:
                return await op()
        raise RuntimeError("unreachable")

    async def get_json(
        self,
        url: str,
        *,
        params: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        async def _do() -> dict[str, Any]:
            r = await self._client.get(url, params=params, headers=headers)
            r.raise_for_status()
            data = r.json()
            if not isinstance(data, dict):
                return {"data": data}
            return data

        return await self._retry(_do)


# ---- Stack Exchange adapter --------------------------------------------

_BASE_URL = "https://api.stackexchange.com/2.3"
_DEFAULT_SITE = "stackoverflow"


def _to_markdown(html: str | None) -> str:
    if not html:
        return ""
    soup = BeautifulSoup(html, "lxml")
    rendered = _markdownify(str(soup), heading_style="ATX")
    return rendered.strip()


class StackExchangeAdapter(_BaseHttpAdapter):
    name = "stackexchange"
    source_kind = SourceKind.FORUM

    def __init__(
        self,
        *,
        site: str | None = None,
        api_key: str | None = None,
    ) -> None:
        super().__init__(base_url=_BASE_URL)
        # STACKEXCHANGE_SITE env knob is new; legacy hardcoded the constructor
        # arg, but the plugin host reads config from env so we expose both.
        self._site = site or os.environ.get("STACKEXCHANGE_SITE") or _DEFAULT_SITE
        self._api_key = (
            api_key if api_key is not None else os.environ.get("STACKEXCHANGE_KEY")
        )

    def _common_params(self) -> dict[str, Any]:
        params: dict[str, Any] = {"site": self._site}
        if self._api_key:
            params["key"] = self._api_key
        return params

    @staticmethod
    def _parse_epoch(value: Any) -> datetime | None:
        if value is None:
            return None
        try:
            return datetime.fromtimestamp(int(value), tz=timezone.utc)
        except (TypeError, ValueError, OSError):
            return None

    def _to_result(self, question: dict[str, Any]) -> SearchResult:
        qid = question.get("question_id")
        title_raw = _first(question.get("title")) or ""
        title = html_lib.unescape(title_raw)
        body_html = question.get("body") or ""
        body_text = re.sub(r"<[^>]+>", " ", body_html)
        body_text = html_lib.unescape(" ".join(body_text.split()))
        snippet = body_text[:500] if body_text else None
        owner = question.get("owner") or {}
        owner_name = _first(owner.get("display_name")) if isinstance(owner, dict) else None
        return SearchResult(
            adapter=self.name,
            external_id=str(qid) if qid is not None else "",
            title=title,
            url=_first(question.get("link")) or "",
            abstract=snippet,
            authors=[owner_name] if owner_name else [],
            published_at=self._parse_epoch(question.get("creation_date")),
            venue=self._site,
            citation_count=question.get("score"),
            raw=question,
            score=question.get("score"),
            source_kind=SourceKind.FORUM,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        page = int(cursor) if cursor and cursor.isdigit() else 1
        params = self._common_params()
        params.update(
            {
                "q": query,
                "sort": "relevance",
                "order": "desc",
                "filter": "withbody",
                "pagesize": max(min(int(limit), 100), 1),
                "page": page,
            }
        )
        if since is not None:
            params["fromdate"] = int(since.replace(tzinfo=timezone.utc).timestamp())
        data = await self.get_json("/search/advanced", params=params)
        items = data.get("items") or []
        results = [self._to_result(q) for q in items if isinstance(q, dict)]
        has_more = bool(data.get("has_more"))
        next_cursor = str(page + 1) if has_more else None
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=data.get("total"),
        )

    async def _fetch_question(self, qid: str) -> dict[str, Any] | None:
        params = self._common_params()
        params["filter"] = "withbody"
        data = await self.get_json(f"/questions/{qid}", params=params)
        items = data.get("items") or []
        if items and isinstance(items[0], dict):
            return items[0]
        return None

    async def _fetch_answers(self, qid: str) -> list[dict[str, Any]]:
        params = self._common_params()
        params.update(
            {
                "filter": "withbody",
                "order": "desc",
                "sort": "votes",
                "pagesize": 5,
            }
        )
        data = await self.get_json(f"/questions/{qid}/answers", params=params)
        items = data.get("items") or []
        return [a for a in items if isinstance(a, dict)]

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        qid = result.external_id
        question = await self._fetch_question(qid)
        answers = await self._fetch_answers(qid)

        title = html_lib.unescape(
            _first((question or {}).get("title")) or result.title or ""
        )
        question_md = _to_markdown((question or {}).get("body"))

        # Order: accepted first, then by score desc.
        accepted_id = (question or {}).get("accepted_answer_id")
        sorted_answers = sorted(
            answers,
            key=lambda a: (
                0 if a.get("answer_id") == accepted_id else 1,
                -(a.get("score") or 0),
            ),
        )[:3]

        parts: list[str] = [f"# {title}\n", "## Question\n", question_md, ""]
        for idx, ans in enumerate(sorted_answers, start=1):
            score = ans.get("score")
            tag = (
                "Accepted Answer"
                if ans.get("answer_id") == accepted_id
                else f"Answer {idx}"
            )
            parts.append(f"## {tag} (score={score})\n")
            parts.append(_to_markdown(ans.get("body")))
            parts.append("")

        body = "\n".join(parts).strip() + "\n"
        return FetchedDoc(
            adapter=self.name,
            external_id=qid,
            url=result.url,
            title=title,
            content_type="text/markdown",
            body=body,
            source_tier="html",
            metadata={
                "site": self._site,
                "answer_count": len(sorted_answers),
                "accepted_answer_id": accepted_id,
            },
        )
