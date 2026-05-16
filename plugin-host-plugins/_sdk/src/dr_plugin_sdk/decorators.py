"""Public decorator surface.

Plugin authors tag their methods with @importer.sync / @search.query /
etc. The @plugin(manifest=...) class decorator walks the class at
decoration time, collects all tagged methods, and stores a registry on
the class that serve.py reads to build the gRPC servicer.

The marker decorators are *not* wrappers — they just attach a small
attribute (_dr_rpc_name) so @plugin can find them. This keeps stack
traces and signatures clean.
"""

from __future__ import annotations

import inspect
import json
import sys
from pathlib import Path
from typing import Any, Callable, TypeVar

_F = TypeVar("_F", bound=Callable[..., Any])

# Class attribute names the SDK stashes things under.
_REGISTRY_ATTR = "_dr_rpc_registry"
_MANIFEST_ATTR = "_dr_manifest"
_VERSION_ATTR = "_dr_plugin_version"
_CAPS_ATTR = "_dr_capabilities"


def _tag(rpc_name: str) -> Callable[[_F], _F]:
    def deco(fn: _F) -> _F:
        # Method may already be tagged for a different RPC — that's a bug.
        existing = getattr(fn, "_dr_rpc_name", None)
        if existing and existing != rpc_name:
            raise TypeError(
                f"{fn.__qualname__} is already tagged as @{existing}; "
                f"cannot also be @{rpc_name}"
            )
        fn._dr_rpc_name = rpc_name  # type: ignore[attr-defined]
        return fn

    return deco


# ----- Marker namespaces ----------------------------------------------
#
# These behave like decorators (``@importer.sync``) but are also callable
# attribute lookups; we use simple sentinel-style classes.


class _Importer:
    list_collections = staticmethod(_tag("ListCollections"))
    sync = staticmethod(_tag("Sync"))


class _Search:
    query = staticmethod(_tag("SearchQuery"))
    fetch = staticmethod(_tag("SearchFetch"))


class _Marketplace:
    query = staticmethod(_tag("MarketplaceQuery"))
    fetch_listing = staticmethod(_tag("MarketplaceFetchListing"))


# Public names re-exported via __init__.
importer = _Importer()
search = _Search()
marketplace = _Marketplace()


# ----- @plugin class decorator ----------------------------------------


# Manifest capability strings -> RPC method names the SDK expects to find.
# A plugin claiming "importer" must register Sync etc. We don't *enforce*
# perfectly because the host already validates capabilities vs manifest;
# we just collect what's there.
_CAPABILITY_HINTS = {
    "importer": ("Sync", "ListCollections"),
    "search": ("SearchQuery", "SearchFetch"),
    "marketplace": ("MarketplaceQuery", "MarketplaceFetchListing"),
}


def plugin(
    *, manifest: str | Path | None = None, version: str = "", capabilities: list[str] | None = None
) -> Callable[[type], type]:
    """Class decorator. Reads the manifest file and registers tagged methods.

    The manifest path is resolved relative to the *calling* module's file
    so plugin authors can write ``@plugin(manifest="plugin.json")``.
    """

    # Capture the caller's __file__ at decoration time (not at decorate-call).
    caller_frame = inspect.stack()[1]
    caller_file = caller_frame.filename

    def _decorate(cls: type) -> type:
        manifest_data: dict[str, Any] = {}
        if manifest is not None:
            mpath = Path(manifest)
            if not mpath.is_absolute():
                mpath = Path(caller_file).parent / mpath
            if not mpath.exists():
                raise FileNotFoundError(
                    f"plugin manifest not found: {mpath} "
                    f"(resolved from caller {caller_file})"
                )
            try:
                manifest_data = json.loads(mpath.read_text())
            except json.JSONDecodeError as e:
                raise ValueError(f"plugin manifest {mpath} is not valid JSON: {e}") from e

        resolved_version = version or manifest_data.get("version", "0.0.0")
        resolved_caps = list(capabilities or manifest_data.get("capabilities", []))

        # Walk the class for tagged methods. We look at the raw function
        # objects in __dict__ so we get the unbound versions to inspect.
        registry: dict[str, str] = {}
        for name, member in cls.__dict__.items():
            rpc = getattr(member, "_dr_rpc_name", None)
            if rpc:
                if rpc in registry:
                    raise TypeError(
                        f"{cls.__name__} has two methods tagged @{rpc}: "
                        f"{registry[rpc]} and {name}"
                    )
                registry[rpc] = name

        setattr(cls, _REGISTRY_ATTR, registry)
        setattr(cls, _MANIFEST_ATTR, manifest_data)
        setattr(cls, _VERSION_ATTR, resolved_version)
        setattr(cls, _CAPS_ATTR, resolved_caps)
        return cls

    return _decorate


def _registry(cls_or_obj: Any) -> dict[str, str]:
    return getattr(cls_or_obj, _REGISTRY_ATTR, {}) or {}


def _version(cls_or_obj: Any) -> str:
    return getattr(cls_or_obj, _VERSION_ATTR, "") or ""


def _capabilities(cls_or_obj: Any) -> list[str]:
    return list(getattr(cls_or_obj, _CAPS_ATTR, ()) or ())


__all__ = ["plugin", "importer", "search", "marketplace"]
