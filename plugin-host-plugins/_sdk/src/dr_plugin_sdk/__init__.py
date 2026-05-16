"""dr_plugin_sdk — Python SDK for deepresearch plugins.

Plugins:
    from dr_plugin_sdk import plugin, importer, search, marketplace, main

Types the user is expected to see:
    PluginItem, Collection, SearchResult, SearchPage, RunSummary
"""

from __future__ import annotations

from .decorators import importer, marketplace, plugin, search
from .host_client import HostClient, make_host_client as host_client
from .serve import main, serve
from .types import (
    Collection,
    FetchedDoc,
    PluginItem,
    RunSummary,
    SearchPage,
    SearchResult,
    SourceKind,
    SyncArgs,
    SyncError,
    SyncEvent,
    SyncProgress,
)

__all__ = [
    "Collection",
    "FetchedDoc",
    "HostClient",
    "PluginItem",
    "RunSummary",
    "SearchPage",
    "SearchResult",
    "SourceKind",
    "SyncArgs",
    "SyncError",
    "SyncEvent",
    "SyncProgress",
    "host_client",
    "importer",
    "main",
    "marketplace",
    "plugin",
    "search",
    "serve",
]
