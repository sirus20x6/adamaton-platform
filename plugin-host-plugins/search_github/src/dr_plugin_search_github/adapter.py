"""GitHub adapter (self-contained copy from legacy app/adapters/github.py).

Defaults to repository search; switch to code search by prefixing the
query with ``code:``. ``fetch()`` returns the README of the matched
repository as markdown. Auth via GITHUB_TOKEN env var if present.
"""

from __future__ import annotations

import base64
import os
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Awaitable, Callable, TypeVar

import httpx
from tenacity import (
    AsyncRetrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)


# ---- Local mirrors of legacy app.adapters.protocol ----------------------


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


# ---- Minimal HTTP base --------------------------------------------------

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


# ---- GitHub adapter (verbatim behavior) ---------------------------------

_BASE_URL = "https://api.github.com"
_ACCEPT_V3 = "application/vnd.github+json"


class GithubAdapter(_BaseHttpAdapter):
    name = "github"
    source_kind = SourceKind.REPO

    def __init__(self, *, token: str | None = None) -> None:
        token = token if token is not None else os.environ.get("GITHUB_TOKEN")
        headers = {
            "Accept": _ACCEPT_V3,
            "X-GitHub-Api-Version": "2022-11-28",
        }
        if token:
            headers["Authorization"] = f"Bearer {token}"
        super().__init__(base_url=_BASE_URL, headers=headers)
        self._token = token

    @staticmethod
    def _parse_date(value: str | None) -> datetime | None:
        if not value:
            return None
        try:
            return datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            return None

    def _repo_to_result(self, repo: dict[str, Any]) -> SearchResult:
        full_name = _first(repo.get("full_name")) or _first(repo.get("name")) or ""
        owner = repo.get("owner") or {}
        owner_name = _first(owner.get("login")) if isinstance(owner, dict) else None
        return SearchResult(
            adapter=self.name,
            external_id=full_name,
            title=_first(repo.get("name")) or full_name,
            url=_first(repo.get("html_url")) or f"https://github.com/{full_name}",
            abstract=_first(repo.get("description")),
            authors=[owner_name] if owner_name else [],
            published_at=self._parse_date(repo.get("created_at")),
            venue="GitHub",
            citation_count=repo.get("stargazers_count"),
            raw=repo,
            score=repo.get("score"),
            source_kind=SourceKind.REPO,
        )

    def _code_to_result(self, item: dict[str, Any]) -> SearchResult:
        repo = item.get("repository") or {}
        repo_full = _first(repo.get("full_name")) or ""
        path = _first(item.get("path")) or ""
        external_id = f"{repo_full}:{path}" if repo_full else path
        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=_first(item.get("name")) or path,
            url=_first(item.get("html_url")) or f"https://github.com/{repo_full}",
            abstract=None,
            authors=[],
            published_at=None,
            venue="GitHub",
            citation_count=None,
            raw=item,
            score=item.get("score"),
            source_kind=SourceKind.REPO,
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
        is_code = query.startswith("code:")
        q = query[len("code:") :].strip() if is_code else query.strip()
        if since is not None and not is_code:
            q = f"{q} pushed:>={since.date().isoformat()}".strip()

        endpoint = "/search/code" if is_code else "/search/repositories"
        params: dict[str, Any] = {
            "q": q,
            "per_page": max(min(int(limit), 100), 1),
            "page": page,
        }
        data = await self.get_json(endpoint, params=params)
        items = data.get("items") or []
        if is_code:
            results = [self._code_to_result(i) for i in items if isinstance(i, dict)]
        else:
            results = [self._repo_to_result(i) for i in items if isinstance(i, dict)]
        total = data.get("total_count")
        # next_cursor only when we actually saw a full page of hits.
        next_cursor = str(page + 1) if len(results) >= int(limit) else None
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=int(total) if isinstance(total, int) else None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        # external_id is owner/repo or owner/repo:path.
        repo_id = (
            result.external_id.split(":", 1)[0]
            if ":" in result.external_id
            else result.external_id
        )
        if "/" not in repo_id:
            body = f"# {result.title}\n\n{result.abstract or ''}\n"
            return FetchedDoc(
                adapter=self.name,
                external_id=result.external_id,
                url=result.url,
                title=result.title,
                content_type="text/markdown",
                body=body,
                source_tier="json",
                metadata={"repo": result.raw},
            )

        readme = await self.get_json(f"/repos/{repo_id}/readme")
        encoding = readme.get("encoding")
        content = readme.get("content") or ""
        if encoding == "base64" and content:
            try:
                decoded = base64.b64decode(content).decode("utf-8", errors="replace")
            except (ValueError, TypeError):
                decoded = ""
        else:
            decoded = content

        if not decoded:
            decoded = f"# {result.title}\n\n{result.abstract or ''}\n"

        return FetchedDoc(
            adapter=self.name,
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/markdown",
            body=decoded,
            source_tier="html",
            metadata={
                "repo": repo_id,
                "readme_path": readme.get("path"),
                "readme_html_url": readme.get("html_url"),
            },
        )
