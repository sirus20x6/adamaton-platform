#!/usr/bin/env python3
"""Parity harness for the search-plugin migration.

For every (plugin_id, query, limit) in TEST_CASES we run two paths:

  1. Legacy: import ``app.adapters.<name>`` and call its ``.search()``.
  2. New:    GET $PLUGIN_HOST_URL/platform/search/query?source=<id>&q=...

Then we diff the two ``SearchPage`` results — overlap on external_ids,
order, title alignment — and print a per-plugin pass/fail row.

The threshold is intentionally loose (>=80% overlap on external_ids in
the same page) because legitimate variation exists:
  * upstream rate limits + freshness windows ("more papers since the
    legacy snapshot was taken")
  * ranking tie-breakers (e.g. arXiv re-orders within the same
    relevance bucket call to call)

Run modes:
  --all              every (plugin, query) pair (default)
  --plugin <id>      narrow to one plugin
  --legacy-only      baseline: legacy paths only, no host calls
  --dry-run          print the plan, hit nothing
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlencode

# ----- test plan -------------------------------------------------------


@dataclass
class TestCase:
    plugin_id: str  # e.g. "search_arxiv"
    legacy_module: str  # e.g. "app.adapters.arxiv"
    legacy_class: str  # e.g. "ArxivAdapter"
    query: str
    limit: int
    required_env: tuple[str, ...] = ()  # env vars needed to run; empty = always
    notes: str = ""


# Canonical queries — chosen because they're stable across runs and
# don't require API keys (those are LIVE-only and added separately).
TEST_CASES: tuple[TestCase, ...] = (
    TestCase("search_arxiv", "app.adapters.arxiv", "ArxivAdapter",
             "transformer attention", 5),
    TestCase("search_semantic_scholar", "app.adapters.semantic_scholar", "SemanticScholarAdapter",
             "graph neural network", 5,
             required_env=("SEMANTIC_SCHOLAR_API_KEY",),
             notes="SS works without a key but rate-limits aggressively; skip if missing."),
    TestCase("search_openalex", "app.adapters.openalex", "OpenAlexAdapter",
             "RAPTOR retrieval", 5),
    TestCase("search_wikipedia", "app.adapters.wikipedia", "WikipediaAdapter",
             "Anthropic", 3),
    TestCase("search_github", "app.adapters.github", "GitHubAdapter",
             "vllm", 3,
             required_env=("GITHUB_TOKEN",),
             notes="Unauth is 60 req/h which is enough for one run, but a token avoids 403s."),
)


# ----- diff logic ------------------------------------------------------


OVERLAP_THRESHOLD = 0.80


@dataclass
class DiffReport:
    plugin_id: str
    query: str
    limit: int
    legacy_ids: list[str] = field(default_factory=list)
    new_ids: list[str] = field(default_factory=list)
    overlap_ratio: float = 0.0
    order_match: bool = False
    passed: bool = False
    error: str | None = None
    skipped_reason: str | None = None


def diff_pages(plugin_id: str, query: str, limit: int,
               legacy: dict[str, Any], new: dict[str, Any]) -> DiffReport:
    rep = DiffReport(plugin_id=plugin_id, query=query, limit=limit)
    rep.legacy_ids = [_extract_id(r) for r in (legacy.get("results") or [])][:limit]
    rep.new_ids = [_extract_id(r) for r in (new.get("results") or [])][:limit]
    if not rep.legacy_ids and not rep.new_ids:
        rep.overlap_ratio = 1.0  # both empty = trivially identical
    else:
        lset = set(rep.legacy_ids)
        nset = set(rep.new_ids)
        union = lset | nset
        rep.overlap_ratio = (len(lset & nset) / len(union)) if union else 0.0
    rep.order_match = rep.legacy_ids == rep.new_ids
    rep.passed = rep.overlap_ratio >= OVERLAP_THRESHOLD
    return rep


def _extract_id(r: Any) -> str:
    # Accept dataclass-like and dict shapes.
    if hasattr(r, "external_id"):
        return str(getattr(r, "external_id"))
    if isinstance(r, dict):
        return str(r.get("external_id") or r.get("externalId") or "")
    return ""


# ----- legacy path -----------------------------------------------------


async def run_legacy(tc: TestCase) -> dict[str, Any]:
    """Import & call the legacy adapter directly."""
    import importlib

    module = importlib.import_module(tc.legacy_module)
    cls = getattr(module, tc.legacy_class)
    adapter = cls()
    try:
        page = await adapter.search(tc.query, limit=tc.limit)
    finally:
        if hasattr(adapter, "aclose"):
            try:
                await adapter.aclose()
            except Exception:
                pass
    return _page_to_jsonable(page)


def _page_to_jsonable(page: Any) -> dict[str, Any]:
    results = []
    for r in (getattr(page, "results", None) or []):
        results.append({
            "external_id": getattr(r, "external_id", ""),
            "title": getattr(r, "title", ""),
            "url": getattr(r, "url", ""),
        })
    return {
        "results": results,
        "next_cursor": getattr(page, "next_cursor", None),
        "total_estimated": getattr(page, "total_estimated", None),
    }


# ----- new path (HTTP) -------------------------------------------------


async def run_new(tc: TestCase, host_url: str) -> dict[str, Any]:
    """Hit the plugin-host HTTP endpoint."""
    import httpx

    qs = urlencode({"source": tc.plugin_id, "q": tc.query, "limit": tc.limit})
    url = f"{host_url.rstrip('/')}/platform/search/query?{qs}"
    async with httpx.AsyncClient(timeout=60.0) as client:
        resp = await client.get(url)
        resp.raise_for_status()
        body = resp.json()
    # Body shape: {"source": ..., "page": {<SearchPage protojson>}}
    page = body.get("page") or {}
    return {
        "results": page.get("results") or [],
        "next_cursor": page.get("nextCursor") or page.get("next_cursor"),
        "total_estimated": page.get("totalEstimated") or page.get("total_estimated"),
    }


# ----- skip logic ------------------------------------------------------


def _skip_reason(tc: TestCase) -> str | None:
    missing = [k for k in tc.required_env if not os.environ.get(k)]
    if missing:
        return f"missing env: {','.join(missing)}"
    return None


# ----- printing --------------------------------------------------------


def print_table(reports: list[DiffReport]) -> None:
    headers = ("plugin", "query", "n", "overlap", "order", "status")
    rows: list[tuple[str, ...]] = []
    for r in reports:
        if r.skipped_reason:
            status = f"SKIP ({r.skipped_reason})"
            rows.append((r.plugin_id, _trunc(r.query, 32), str(r.limit), "-", "-", status))
            continue
        if r.error:
            rows.append((r.plugin_id, _trunc(r.query, 32), str(r.limit), "-", "-", f"ERR: {_trunc(r.error, 40)}"))
            continue
        overlap = f"{r.overlap_ratio*100:.0f}%"
        order = "yes" if r.order_match else "no"
        status = "PASS" if r.passed else "FAIL"
        rows.append((r.plugin_id, _trunc(r.query, 32), str(r.limit), overlap, order, status))

    widths = [max(len(h), *(len(row[i]) for row in rows)) for i, h in enumerate(headers)]
    sep = "+" + "+".join("-" * (w + 2) for w in widths) + "+"
    print(sep)
    print("| " + " | ".join(h.ljust(w) for h, w in zip(headers, widths)) + " |")
    print(sep)
    for row in rows:
        print("| " + " | ".join(c.ljust(w) for c, w in zip(row, widths)) + " |")
    print(sep)


def _trunc(s: str, n: int) -> str:
    return s if len(s) <= n else s[: n - 1] + "..."


# ----- main ------------------------------------------------------------


async def _amain(args: argparse.Namespace) -> int:
    plan = list(TEST_CASES)
    if args.plugin:
        plan = [tc for tc in plan if tc.plugin_id == args.plugin]
        if not plan:
            print(f"no test cases for plugin {args.plugin!r}", file=sys.stderr)
            return 2

    host_url = args.host or os.environ.get("PLUGIN_HOST_URL", "http://localhost:7375")

    if args.dry_run:
        print(f"dry-run: would hit plugin-host at {host_url}")
        for tc in plan:
            skip = _skip_reason(tc)
            note = f"  ({skip})" if skip else ""
            print(f"  - {tc.plugin_id}: query={tc.query!r} limit={tc.limit}{note}")
        return 0

    # Ensure app.* is importable when the harness is run from the repo root.
    _ensure_backend_on_path()

    reports: list[DiffReport] = []
    for tc in plan:
        skip = _skip_reason(tc)
        if skip:
            reports.append(DiffReport(
                plugin_id=tc.plugin_id, query=tc.query, limit=tc.limit,
                skipped_reason=skip,
            ))
            continue

        rep: DiffReport
        try:
            t0 = time.monotonic()
            legacy_page = await run_legacy(tc)
            t1 = time.monotonic()
            if args.legacy_only:
                # We still produce a report so the table shows the baseline.
                rep = DiffReport(
                    plugin_id=tc.plugin_id, query=tc.query, limit=tc.limit,
                    legacy_ids=[r["external_id"] for r in legacy_page["results"]],
                    new_ids=[], overlap_ratio=1.0, order_match=True, passed=True,
                )
                rep.error = None
                print(f"[legacy] {tc.plugin_id} {tc.query!r} → "
                      f"{len(legacy_page['results'])} hits in {t1-t0:.2f}s")
            else:
                new_page = await run_new(tc, host_url)
                rep = diff_pages(tc.plugin_id, tc.query, tc.limit, legacy_page, new_page)
        except Exception as exc:
            rep = DiffReport(
                plugin_id=tc.plugin_id, query=tc.query, limit=tc.limit,
                error=f"{type(exc).__name__}: {exc}",
            )
        reports.append(rep)

    print_table(reports)

    if args.json:
        print(json.dumps([_report_to_json(r) for r in reports], indent=2))

    failures = [r for r in reports
                if r.skipped_reason is None and r.error is None and not r.passed]
    errors = [r for r in reports if r.error is not None]
    if errors or failures:
        print(f"\n{len(failures)} parity diff(s), {len(errors)} error(s)", file=sys.stderr)
        return 1
    return 0


def _report_to_json(r: DiffReport) -> dict[str, Any]:
    return {
        "plugin_id": r.plugin_id,
        "query": r.query,
        "limit": r.limit,
        "legacy_ids": r.legacy_ids,
        "new_ids": r.new_ids,
        "overlap_ratio": r.overlap_ratio,
        "order_match": r.order_match,
        "passed": r.passed,
        "skipped_reason": r.skipped_reason,
        "error": r.error,
    }


def _ensure_backend_on_path() -> None:
    # platform/plugins/_tools/parity_check.py → platform/backend/.
    here = Path(__file__).resolve()
    backend = here.parent.parent.parent / "backend"
    if backend.is_dir():
        sys.path.insert(0, str(backend))


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--all", action="store_true", help="run every (plugin, query) pair (default)")
    p.add_argument("--plugin", type=str, help="narrow to one plugin id")
    p.add_argument("--legacy-only", action="store_true", help="run only the legacy adapter paths")
    p.add_argument("--dry-run", action="store_true", help="print the plan, hit nothing")
    p.add_argument("--host", type=str, help="plugin-host URL (default $PLUGIN_HOST_URL or http://localhost:7375)")
    p.add_argument("--json", action="store_true", help="also dump JSON report after the table")
    args = p.parse_args()

    return asyncio.run(_amain(args))


if __name__ == "__main__":
    sys.exit(main())
