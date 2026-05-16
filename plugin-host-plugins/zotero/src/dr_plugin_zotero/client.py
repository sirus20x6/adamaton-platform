"""Zotero Web API client.

We use ``pyzotero`` for the heavy lifting (auth, pagination, a few
edge-cases like the ``/everything`` iterator) but it's a sync library,
so every call gets wrapped in :func:`asyncio.get_running_loop().run_in_executor`
to keep the rest of our pipeline async-friendly.

Where ``pyzotero`` doesn't expose what we need we fall back to a plain
``httpx`` request — for example we need the ``Last-Modified-Version``
response header to track the incremental ``since`` cursor and the
``Retry-After`` header on 429s.

The client honours Zotero's rate limits: any 429 response triggers a
``Retry-After``-driven sleep + exponential-backoff retry via tenacity.

Public surface mirrors the spec:

* :meth:`ZoteroClient.iter_items` — async iterator over normalized item
  envelopes (Web API JSON format, suitable for
  :func:`app.zotero.normalize.normalize_item`).
* :meth:`ZoteroClient.get_attachment_pdf_url` — child PDF link for an item.
* :meth:`ZoteroClient.download_attachment` — fetch the bytes of that PDF.
* :meth:`ZoteroClient.latest_version` — read the library's monotonic
  ``Last-Modified-Version``.
* :meth:`ZoteroClient.verify` — quick connectivity check.
"""

from __future__ import annotations

import asyncio
import logging
from typing import AsyncIterator, Literal

import httpx
from tenacity import (
    retry,
    retry_if_exception_type,
    stop_after_attempt,
    wait_exponential,
)

logger = logging.getLogger(__name__)

# Zotero's request quotas are documented at
# https://www.zotero.org/support/dev/web_api/v3/basics — we batch up to
# 100 items per request and stop on the first empty page. Defaults align
# with pyzotero's own limit.
_PAGE_LIMIT = 100
_BASE_URL = "https://api.zotero.org"


class ZoteroAuthError(RuntimeError):
    """Raised for 401 / 403 responses (bad API key or no library access)."""


class ZoteroNotFoundError(RuntimeError):
    """Raised when the library_id can't be resolved (404 from upstream)."""


class ZoteroRateLimited(RuntimeError):
    """Raised when retries on 429 are exhausted."""


class ZoteroClient:
    """Thin async wrapper around :class:`pyzotero.zotero.Zotero`.

    The wrapper exists so the orchestrator can use ``async for`` without
    pulling pyzotero into every code path that needs a Zotero envelope
    (the local sqlite reader doesn't depend on it at all).
    """

    def __init__(
        self,
        api_key: str,
        library_type: Literal["user", "group"],
        library_id: str,
        *,
        timeout: float = 30.0,
    ) -> None:
        if library_type not in {"user", "group"}:
            raise ValueError(
                f"library_type must be 'user' or 'group' (got {library_type!r})"
            )
        if not api_key:
            raise ValueError("api_key must be a non-empty string")
        if not library_id:
            raise ValueError("library_id must be a non-empty string")

        self._api_key = api_key
        self._library_type = library_type
        self._library_id = str(library_id)
        self._timeout = timeout
        # Lazy: only built on the first call so unit tests can construct
        # a client without ``pyzotero`` installed.
        self._zot = None
        self._http: httpx.AsyncClient | None = None

    @property
    def library_url(self) -> str:
        """Base path for the library's REST endpoints."""

        kind = "users" if self._library_type == "user" else "groups"
        return f"{_BASE_URL}/{kind}/{self._library_id}"

    async def aclose(self) -> None:
        """Close the underlying ``httpx.AsyncClient`` if we built one."""

        if self._http is not None:
            try:
                await self._http.aclose()
            except Exception:  # pragma: no cover - shutdown best-effort
                logger.warning("zotero: error closing httpx client", exc_info=True)
            self._http = None

    async def __aenter__(self) -> ZoteroClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def verify(self) -> dict[str, object]:
        """Quick connectivity probe; returns ``{ok, library_name, item_count}``.

        Maps any 4xx into a structured ``{ok: False, error}`` payload so
        the API layer can hand the user a clean error message instead of
        a 500.
        """

        try:
            payload = await self._json(
                "GET",
                f"{self.library_url}/items",
                params={"limit": 1, "format": "json"},
            )
        except ZoteroAuthError as exc:
            return {"ok": False, "error": f"auth failed: {exc}"}
        except ZoteroNotFoundError as exc:
            return {"ok": False, "error": f"library not found: {exc}"}
        except httpx.HTTPError as exc:
            return {"ok": False, "error": f"http error: {exc}"}

        # We don't have a cheap "library name" endpoint for /users; pull
        # whatever the items envelope hands back.
        item_count = (
            payload.get("totalResults") if isinstance(payload, dict) else None
        )
        if item_count is None and isinstance(payload, dict):
            item_count = payload.get("total")

        # Try /<library>/keys/<key> for the library name; this is best
        # effort, the verify call is mostly about confirming auth works.
        library_name = None
        try:
            ident = await self._json("GET", f"{_BASE_URL}/keys/{self._api_key}")
            if isinstance(ident, dict):
                library_name = ident.get("username") or ident.get("name")
        except Exception:  # pragma: no cover - defensive
            library_name = None

        return {
            "ok": True,
            "library_name": library_name,
            "item_count": int(item_count) if isinstance(item_count, int) else None,
            "library_type": self._library_type,
            "library_id": self._library_id,
        }

    async def latest_version(self) -> int:
        """Read the library's ``Last-Modified-Version`` header.

        The header is monotonic per library and is the cursor we hand
        back to ``iter_items(since=...)`` for incremental sync.
        """

        client = await self._client()
        url = f"{self.library_url}/items"
        # ``limit=1`` keeps the body small; we only care about the header.
        try:
            response = await client.get(url, params={"limit": 1, "format": "versions"})
        except httpx.HTTPError as exc:
            logger.warning("zotero: latest_version failed: %s", exc)
            raise

        _check_status(response)
        version = response.headers.get("Last-Modified-Version")
        if version is None:
            return 0
        try:
            return int(version)
        except (TypeError, ValueError):
            return 0

    async def iter_items(
        self,
        *,
        since: int | None = None,
        item_type: str | None = None,
    ) -> AsyncIterator[dict]:
        """Iterate the library's top-level items.

        ``since`` is the Zotero modification version cursor — only items
        whose ``version`` is greater than this value are returned. Pass
        the result of a prior :meth:`latest_version` call.
        """

        client = await self._client()
        start = 0

        while True:
            params: dict[str, object] = {
                "limit": _PAGE_LIMIT,
                "start": start,
                "format": "json",
                "include": "data,bib",
                "itemType": "-attachment",  # exclude bare attachments at top level
            }
            if since is not None:
                params["since"] = int(since)
            if item_type is not None:
                params["itemType"] = item_type

            response = await self._request(
                client, "GET", f"{self.library_url}/items", params=params
            )
            payload = response.json()
            if not isinstance(payload, list) or not payload:
                return

            for entry in payload:
                if isinstance(entry, dict):
                    yield entry

            start += len(payload)
            # When the server returns less than the page size we've
            # walked the whole library.
            if len(payload) < _PAGE_LIMIT:
                return

    async def get_attachment_pdf_url(self, item_key: str) -> str | None:
        """Find a child attachment with a PDF content type and return its file URL.

        Zotero stores PDFs as child items of the parent reference. We
        list the children, pick the first ``application/pdf`` one (if
        any) and return the URL the bytes can be downloaded from.
        """

        url = f"{self.library_url}/items/{item_key}/children"
        try:
            payload = await self._json("GET", url, params={"format": "json"})
        except (ZoteroNotFoundError, ZoteroAuthError):
            return None
        if not isinstance(payload, list):
            return None
        for entry in payload:
            if not isinstance(entry, dict):
                continue
            data = entry.get("data") or {}
            if data.get("itemType") != "attachment":
                continue
            if data.get("contentType") != "application/pdf":
                continue
            attachment_key = data.get("key") or entry.get("key")
            if not attachment_key:
                continue
            return f"{self.library_url}/items/{attachment_key}/file"
        return None

    async def download_attachment(self, item_key: str) -> bytes | None:
        """Fetch the raw bytes of an item's first PDF attachment, if any."""

        pdf_url = await self.get_attachment_pdf_url(item_key)
        if pdf_url is None:
            return None
        client = await self._client()
        response = await self._request(client, "GET", pdf_url)
        return response.content

    # ------------------------------------------------------------------
    # Plumbing
    # ------------------------------------------------------------------

    async def _client(self) -> httpx.AsyncClient:
        if self._http is None:
            self._http = httpx.AsyncClient(
                timeout=self._timeout,
                headers={
                    "Zotero-API-Version": "3",
                    "Zotero-API-Key": self._api_key,
                    "User-Agent": "deepresearch-platform/0.1 (+https://github.com)",
                },
            )
        return self._http

    @retry(
        retry=retry_if_exception_type((ZoteroRateLimited, httpx.TransportError)),
        stop=stop_after_attempt(5),
        wait=wait_exponential(multiplier=1.0, min=1.0, max=30.0),
        reraise=True,
    )
    async def _request(
        self,
        client: httpx.AsyncClient,
        method: str,
        url: str,
        **kwargs: object,
    ) -> httpx.Response:
        response = await client.request(method, url, **kwargs)  # type: ignore[arg-type]
        if response.status_code == 429:
            # Honour Retry-After verbatim, then bubble so tenacity gets a
            # chance to back off as well.
            retry_after = response.headers.get("Retry-After", "1")
            try:
                seconds = float(retry_after)
            except (TypeError, ValueError):
                seconds = 1.0
            seconds = max(0.0, min(seconds, 30.0))
            logger.info("zotero: 429 rate limited, sleeping %.1fs", seconds)
            await asyncio.sleep(seconds)
            raise ZoteroRateLimited(f"429 — retry after {seconds:.1f}s")
        _check_status(response)
        return response

    async def _json(self, method: str, url: str, **kwargs: object) -> object:
        client = await self._client()
        response = await self._request(client, method, url, **kwargs)
        try:
            return response.json()
        except ValueError:
            return None


def _check_status(response: httpx.Response) -> None:
    """Raise the matching wrapper error for non-2xx responses."""

    if response.status_code in (401, 403):
        raise ZoteroAuthError(
            f"{response.status_code} {response.reason_phrase}: {response.text[:200]}"
        )
    if response.status_code == 404:
        raise ZoteroNotFoundError(f"404: {response.text[:200]}")
    if 400 <= response.status_code < 600 and response.status_code != 429:
        # Let httpx build its own structured exception for unexpected errors.
        response.raise_for_status()


__all__ = [
    "ZoteroAuthError",
    "ZoteroClient",
    "ZoteroNotFoundError",
    "ZoteroRateLimited",
]
