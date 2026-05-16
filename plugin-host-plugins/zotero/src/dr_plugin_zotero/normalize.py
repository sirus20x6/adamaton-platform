"""Normalize a Zotero item dict into our internal :class:`ZoteroItemNormalized`.

The Web API and the local sqlite reader hand us slightly different
shapes, so both call into the same path here. Items typically arrive
as the Zotero JSON envelope::

    {
      "key": "ABCD1234",
      "version": 1234,
      "data": {
          "itemType": "journalArticle",
          "title": "Some Paper",
          "DOI": "10.1234/abc.def",
          "url": "https://arxiv.org/abs/2401.00001",
          "extra": "arXiv:2401.00001v2 [cs.CL]",
          "ISBN": "978-0-12-345678-9",
          "creators": [
              {"creatorType": "author", "lastName": "Smith", "firstName": "A."},
              ...
          ],
          "date": "2024-01-15",
          ...
      },
      "links": {"attachment": {...}},
      ...
    }

The sqlite reader builds the same envelope so this module is the only
place that knows the wire shape.

Public surface:

* :class:`ZoteroItemNormalized` — slotted dataclass holding the
  identifiers + metadata that downstream code actually cares about.
* :func:`normalize_item` — turn the raw dict into the dataclass.
* The DOI / arXiv / ISBN / content-hash extractors are exported
  individually so the unit tests can hit them directly.
"""

from __future__ import annotations

import hashlib
import re
import unicodedata
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

# DOIs are matched anywhere in a string. The constraint is loose enough
# to catch inline references like ``DOI: 10.1234/abc-def`` while still
# refusing to hit things like ``10.x.y`` that aren't real registrants.
_DOI_RE = re.compile(r"\b(10\.\d{4,9}/[^\s\"<>]+)", re.IGNORECASE)
_DOI_PREFIX_RE = re.compile(r"^(?:https?://)?(?:dx\.)?doi\.org/", re.IGNORECASE)

# Modern arXiv ids (post-2007): ``YYMM.NNNNN`` with optional version.
_ARXIV_NEW_RE = re.compile(r"\b(\d{4}\.\d{4,5})(?:v\d+)?\b")
# Legacy ids (pre-2007): ``hep-th/0301001`` shape.
_ARXIV_OLD_RE = re.compile(r"\b([a-z\-]+(?:\.[A-Z]{2})?/\d{7})(?:v\d+)?\b")
# ``arXiv: 2401.00001`` / ``arXiv:2401.00001v2`` markers in the ``extra``
# field. Captures the bare id.
_ARXIV_TAG_RE = re.compile(
    r"arXiv\s*[:\s]\s*(\d{4}\.\d{4,5}|[a-z\-]+(?:\.[A-Z]{2})?/\d{7})(?:v\d+)?",
    re.IGNORECASE,
)
# Final validation patterns for the canonical form we store.
_ARXIV_VALID_NEW = re.compile(r"^\d{4}\.\d{4,5}$")
_ARXIV_VALID_OLD = re.compile(r"^[a-z\-]+(?:\.[A-Z]{2})?/\d{7}$")

# ISBN: 10 or 13 digits with optional X check digit on ISBN-10.
_ISBN_RE = re.compile(r"\b(?=(?:\d[\s\-]?){9,12}[\dXx]\b)([\d\-\sXx]+)")


@dataclass(slots=True, kw_only=True)
class ZoteroItemNormalized:
    """A Zotero item reduced to the fields our pipeline actually uses."""

    zotero_key: str
    title: str | None
    authors: list[str] = field(default_factory=list)
    year: int | None
    doi: str | None
    arxiv_id: str | None
    isbn: str | None
    content_hash: bytes
    item_type: str
    has_pdf: bool
    pdf_url: str | None = None
    pdf_local_path: Path | None = None
    raw: dict[str, Any] = field(default_factory=dict)

    @property
    def canonical_kind(self) -> str:
        """Pick the strongest identifier we have for dedup."""

        if self.doi:
            return "doi"
        if self.arxiv_id:
            return "arxiv"
        if self.isbn:
            return "isbn"
        if self.content_hash:
            return "content_hash"
        return "zotero_only"

    @property
    def canonical_id(self) -> str:
        """The string form of the canonical id, suitable for indexing."""

        if self.doi:
            return self.doi
        if self.arxiv_id:
            return self.arxiv_id
        if self.isbn:
            return self.isbn
        if self.content_hash:
            return self.content_hash.hex()
        # Last resort: scope by zotero_key so the row still has a unique
        # canonical_id (the UNIQUE constraint is on ``zotero_user_id, zotero_key``
        # so two zotero-only rows from different libraries don't collide here).
        return f"zotero:{self.zotero_key}"


def normalize_item(
    item: dict[str, Any],
    *,
    pdf_url: str | None = None,
    pdf_local_path: Path | None = None,
) -> ZoteroItemNormalized:
    """Coerce a raw Zotero JSON envelope into :class:`ZoteroItemNormalized`."""

    data = item.get("data") if isinstance(item, dict) else None
    if not isinstance(data, dict):
        # Some API responses hand back a flat envelope; treat ``item``
        # itself as the data dict in that case.
        data = item if isinstance(item, dict) else {}

    key = str(item.get("key") or data.get("key") or "")
    item_type = str(data.get("itemType") or "")
    title = _coerce_title(data.get("title"))

    authors = _extract_authors(data.get("creators") or [])

    year = _extract_year(data.get("date"))
    doi = extract_doi(data)
    arxiv_id = extract_arxiv_id(data)
    isbn = extract_isbn(data)

    has_pdf = _detect_pdf(item, data, pdf_url=pdf_url, pdf_local_path=pdf_local_path)

    content_hash = compute_content_hash(title, authors, year)

    return ZoteroItemNormalized(
        zotero_key=key,
        title=title,
        authors=authors,
        year=year,
        doi=doi,
        arxiv_id=arxiv_id,
        isbn=isbn,
        content_hash=content_hash,
        item_type=item_type,
        has_pdf=has_pdf,
        pdf_url=pdf_url,
        pdf_local_path=pdf_local_path,
        raw=item if isinstance(item, dict) else {},
    )


def extract_doi(data: dict[str, Any]) -> str | None:
    """Pull a DOI out of the Zotero ``data`` block.

    Lookup order: ``DOI`` field first; then scan ``url`` and ``extra``
    for an embedded ``10.\\d{4,9}/...`` pattern. The result is lowercased
    and stripped of any ``https://doi.org/`` prefix so the value matches
    the form R2R metadata stores.
    """

    candidate = (data.get("DOI") or data.get("doi") or "").strip()
    if candidate:
        cleaned = _clean_doi(candidate)
        if cleaned:
            return cleaned

    for haystack_key in ("url", "extra"):
        haystack = data.get(haystack_key)
        if not isinstance(haystack, str):
            continue
        match = _DOI_RE.search(haystack)
        if match:
            cleaned = _clean_doi(match.group(1))
            if cleaned:
                return cleaned
    return None


def extract_arxiv_id(data: dict[str, Any]) -> str | None:
    """Pull an arXiv id out of the Zotero ``data`` block.

    Sources we check (in order):

    * ``url`` — typical pattern ``http://arxiv.org/abs/2401.00001``;
    * ``extra`` — entries like ``arXiv: 2401.00001v2`` or
      ``arXiv:hep-th/0301001``;
    * any other free-text field that happens to embed an id.

    Version suffixes are stripped before validation. Returns ``None`` if
    nothing parses to either the modern or legacy shape.
    """

    for haystack_key in ("url", "extra", "archiveID", "callNumber"):
        haystack = data.get(haystack_key)
        if not isinstance(haystack, str) or not haystack:
            continue

        # 1. Explicit "arXiv:..." marker — tightest match.
        marker = _ARXIV_TAG_RE.search(haystack)
        if marker:
            candidate = marker.group(1)
            valid = _validate_arxiv(candidate)
            if valid:
                return valid

        # 2. URL-style or bare-id new-form.
        match = _ARXIV_NEW_RE.search(haystack)
        if match:
            valid = _validate_arxiv(match.group(1))
            if valid:
                return valid

        # 3. Legacy form.
        match = _ARXIV_OLD_RE.search(haystack)
        if match:
            valid = _validate_arxiv(match.group(1))
            if valid:
                return valid

    return None


def extract_isbn(data: dict[str, Any]) -> str | None:
    """Normalize an ISBN by stripping dashes / spaces and uppercasing."""

    raw = data.get("ISBN") or data.get("isbn") or ""
    if not isinstance(raw, str) or not raw.strip():
        return None
    candidates = [
        m.group(1) for m in _ISBN_RE.finditer(raw)
    ]
    for candidate in candidates:
        normalized = re.sub(r"[\s\-]", "", candidate).upper()
        if len(normalized) in (10, 13):
            return normalized
    # Fall back to a plain strip-and-upper on the raw value when the
    # regex doesn't latch (some users paste "ISBN 0306406152" etc.).
    fallback = re.sub(r"[\s\-]", "", raw).upper()
    if len(fallback) in (10, 13) and re.fullmatch(r"\d{9}[\dX]|\d{13}", fallback):
        return fallback
    return None


def compute_content_hash(
    title: str | None,
    authors: list[str],
    year: int | None,
) -> bytes:
    """Last-resort dedup hash: ``sha256(title|lastname|year)``.

    Both the Web API and the sqlite reader feed in slightly different
    string shapes (curly quotes vs straight, NFC vs NFKD), so we
    NFKD-normalize and lowercase before hashing to make the value
    stable across environments.
    """

    title_norm = (
        unicodedata.normalize("NFKD", (title or "").lower().strip())
        .encode("utf-8", errors="ignore")
    )
    last_author = authors[-1] if authors else ""
    # Use the lastname for stability — a paper still hashes to the same
    # thing if the user edits the firstname or initials in Zotero.
    lastname = _last_lastname(last_author).lower()
    year_part = str(year) if year is not None else ""

    parts = b"|".join(
        [
            title_norm,
            lastname.encode("utf-8", errors="ignore"),
            year_part.encode("utf-8", errors="ignore"),
        ]
    )
    return hashlib.sha256(parts).digest()


# ----------------------------------------------------------------------------
# Internal helpers
# ----------------------------------------------------------------------------


def _clean_doi(value: str) -> str | None:
    cleaned = _DOI_PREFIX_RE.sub("", value).strip().rstrip("/.,;)")
    cleaned = cleaned.lower()
    # Validate against the structural regex — ``10.<registrant>/<suffix>``
    # with at least 4 registrant digits and a non-empty suffix.
    match = _DOI_RE.fullmatch(cleaned)
    if match:
        return cleaned
    # Fallback: a longer string (citation, etc.) that *contains* a DOI.
    embedded = _DOI_RE.search(cleaned)
    if embedded:
        return embedded.group(1).lower()
    return None


def _validate_arxiv(value: str) -> str | None:
    bare = value.strip().lower() if "/" in value else value.strip()
    # The legacy form is case-sensitive (``hep-th/0301001`` lowercase but
    # subject classifier may be mixed). The valid regex below tolerates
    # both shapes.
    if "/" in bare:
        # Legacy ids: keep case as-is for the subject class portion but
        # the regex tolerates either way.
        if _ARXIV_VALID_OLD.fullmatch(value):
            return value
        # Try the lowercased variant.
        lowered = value.lower()
        if _ARXIV_VALID_OLD.fullmatch(lowered):
            return lowered
        return None
    if _ARXIV_VALID_NEW.fullmatch(bare):
        return bare
    return None


def _coerce_title(value: Any) -> str | None:
    if value is None:
        return None
    text = str(value).strip()
    return text or None


def _extract_authors(creators: list[Any]) -> list[str]:
    """Build a list of "First Last" / "Last" strings from Zotero creators.

    Zotero uses ``creatorType=author``; we also keep editors / contributors
    out so the content hash isn't poisoned by a "Smith (ed.)" line.
    """

    out: list[str] = []
    for entry in creators:
        if not isinstance(entry, dict):
            continue
        creator_type = (entry.get("creatorType") or "").lower()
        if creator_type and creator_type not in {"author", "creator"}:
            continue
        last = (entry.get("lastName") or "").strip()
        first = (entry.get("firstName") or "").strip()
        # Handle single-name authors stored under ``name``.
        single = (entry.get("name") or "").strip()
        if last and first:
            out.append(f"{first} {last}")
        elif last:
            out.append(last)
        elif single:
            out.append(single)
    return out


def _extract_year(date_value: Any) -> int | None:
    if date_value is None:
        return None
    text = str(date_value).strip()
    if not text:
        return None
    match = re.search(r"(\d{4})", text)
    if not match:
        return None
    try:
        year = int(match.group(1))
    except ValueError:
        return None
    if 1500 <= year <= 2999:
        return year
    return None


def _detect_pdf(
    item: dict[str, Any],
    data: dict[str, Any],
    *,
    pdf_url: str | None,
    pdf_local_path: Path | None,
) -> bool:
    if pdf_url or pdf_local_path:
        return True
    # Web API: ``links.attachment`` exists when the item has a child PDF.
    links = item.get("links") if isinstance(item, dict) else None
    if isinstance(links, dict):
        attachment = links.get("attachment")
        if isinstance(attachment, dict) and attachment.get("href"):
            return True
    # SQLite path: the reader pre-stamps ``has_pdf`` in data when it
    # found a child ``itemAttachments`` row. Honour either flag form.
    if data.get("has_pdf") is True:
        return True
    if data.get("hasPDF") is True:
        return True
    return False


def _last_lastname(value: str) -> str:
    """Pull a single-token lastname out of a ``First Last`` author string."""

    if not value:
        return ""
    tokens = value.strip().split()
    if not tokens:
        return ""
    return tokens[-1]


__all__ = [
    "ZoteroItemNormalized",
    "compute_content_hash",
    "extract_arxiv_id",
    "extract_doi",
    "extract_isbn",
    "normalize_item",
]
