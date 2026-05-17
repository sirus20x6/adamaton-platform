"""World Bank Documents & Reports (WDS) API adapter.

Hits the public WDS search endpoint:

    https://search.worldbank.org/api/v2/wds

The response payload has a quirky shape: ``documents`` is a dict keyed by
the World Bank document id, plus a special ``facets`` entry that we skip.
Pagination uses ``os`` (offset) + ``rows``.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


WDS_URL = "https://search.worldbank.org/api/v2/wds"
USER_AGENT = (
    "adamaton-deepresearch/search_worldbank "
    "(+https://github.com/sirus20x6)"
)
DOC_URL_PREFIX = "https://documents.worldbank.org/curated/en/"

# Fields we request explicitly via &fl= to keep responses lean.
WDS_FIELDS = "docdt,display_title,abstracts,authors,docty,url_friendly_title"


class WorldBankAdapter:
    name = "worldbank"
    source_kind = SourceKind.WEB

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    @staticmethod
    def _parse_authors(raw: Any) -> list[str]:
        """``authors`` can be a string ("Foo; Bar"), a list, or absent."""
        if raw is None:
            return []
        if isinstance(raw, list):
            out: list[str] = []
            for a in raw:
                if isinstance(a, str):
                    name = a.strip()
                elif isinstance(a, dict):
                    name = (a.get("name") or a.get("authorName") or "").strip()
                else:
                    name = str(a).strip()
                if name:
                    out.append(name)
            return out
        if isinstance(raw, str):
            # WB sometimes returns "Smith, John;Doe, Jane" or with newlines.
            parts = [p.strip() for p in raw.replace("\n", ";").split(";")]
            return [p for p in parts if p]
        return []

    @staticmethod
    def _parse_abstract(raw: Any) -> str:
        """``abstracts`` is awkward: typically a dict with a ``cdata!`` key,
        sometimes a list of such dicts, sometimes a bare string.
        """
        if raw is None:
            return ""
        if isinstance(raw, str):
            return raw.strip()
        if isinstance(raw, dict):
            for key in ("cdata!", "cdata", "text", "value"):
                v = raw.get(key)
                if isinstance(v, str) and v.strip():
                    return v.strip()
            # Fallback: first stringy value.
            for v in raw.values():
                if isinstance(v, str) and v.strip():
                    return v.strip()
            return ""
        if isinstance(raw, list) and raw:
            return WorldBankAdapter._parse_abstract(raw[0])
        return ""

    @staticmethod
    def _parse_docdt(raw: Any) -> datetime | None:
        if not isinstance(raw, str) or not raw.strip():
            return None
        text = raw.strip()
        # WDS commonly returns ISO-8601 with timezone, sometimes just a date.
        candidates = [text]
        if text.endswith("Z"):
            candidates.append(text[:-1] + "+00:00")
        for c in candidates:
            try:
                return datetime.fromisoformat(c)
            except ValueError:
                pass
        for fmt in ("%Y-%m-%dT%H:%M:%S", "%Y-%m-%d", "%Y/%m/%d"):
            try:
                return datetime.strptime(text, fmt)
            except ValueError:
                continue
        return None

    @staticmethod
    def _doc_url(doc_id: str, doc: dict[str, Any]) -> str:
        slug = doc.get("url_friendly_title")
        if isinstance(slug, str) and slug.strip():
            return f"{DOC_URL_PREFIX}{slug.strip().lstrip('/')}"
        # Fall back to a generic WB curated landing URL by id.
        return f"{DOC_URL_PREFIX}{doc_id}"

    def _to_result(self, doc_id: str, doc: dict[str, Any]) -> SearchResult:
        title = (doc.get("display_title") or "").strip()
        venue = (doc.get("docty") or "").strip()
        url = self._doc_url(doc_id, doc)
        return SearchResult(
            adapter=self.name,
            external_id=doc_id,
            title=title,
            url=url,
            abstract=self._parse_abstract(doc.get("abstracts")),
            authors=self._parse_authors(doc.get("authors")),
            published_at=self._parse_docdt(doc.get("docdt")),
            venue=venue,
            raw=doc,
            source_kind=SourceKind.WEB,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        rows = max(int(limit), 1)
        offset = 0
        if cursor:
            try:
                offset = max(int(cursor), 0)
            except (TypeError, ValueError):
                offset = 0

        params: dict[str, Any] = {
            "qterm": query,
            "format": "json",
            "rows": rows,
            "os": offset,
            "fl": WDS_FIELDS,
        }
        if since is not None:
            params["strdate"] = since.strftime("%Y-%m-%d")

        resp = await self._client.get(WDS_URL, params=params)
        resp.raise_for_status()
        payload = resp.json() or {}

        documents = payload.get("documents") or {}
        results: list[SearchResult] = []
        if isinstance(documents, dict):
            for doc_id, doc in documents.items():
                if doc_id == "facets":
                    continue
                if not isinstance(doc, dict):
                    continue
                results.append(self._to_result(doc_id, doc))

        total_estimated = 0
        total_raw = payload.get("total")
        try:
            if total_raw is not None:
                total_estimated = int(total_raw)
        except (TypeError, ValueError):
            total_estimated = 0

        next_offset = offset + len(results)
        if not results or (total_estimated and next_offset >= total_estimated):
            next_cursor = ""
        elif len(results) < rows:
            next_cursor = ""
        else:
            next_cursor = str(next_offset)

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
