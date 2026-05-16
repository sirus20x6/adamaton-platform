"""Decorator semantics: tagging, manifest loading, error paths."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from dr_plugin_sdk import importer, plugin, search
from dr_plugin_sdk.decorators import _capabilities, _registry, _version


def test_tag_marks_method() -> None:
    @importer.sync
    async def my_sync(self, args, *, emit, host):
        return None

    assert getattr(my_sync, "_dr_rpc_name", None) == "Sync"


def test_double_tag_with_different_rpc_raises() -> None:
    async def fn(self, args, *, emit, host):
        return None

    importer.sync(fn)
    with pytest.raises(TypeError):
        search.query(fn)


def test_plugin_collects_registry(tmp_path: Path) -> None:
    manifest = tmp_path / "plugin.json"
    manifest.write_text(json.dumps({"version": "1.2.3", "capabilities": ["importer"]}))

    @plugin(manifest=str(manifest))
    class P:
        @importer.sync
        async def sync(self, args, *, emit, host):
            return None

        @importer.list_collections
        async def cols(self, options):
            return []

    reg = _registry(P)
    assert reg == {"Sync": "sync", "ListCollections": "cols"}
    assert _version(P) == "1.2.3"
    assert _capabilities(P) == ["importer"]


def test_plugin_missing_manifest_raises(tmp_path: Path) -> None:
    with pytest.raises(FileNotFoundError):
        @plugin(manifest=str(tmp_path / "nope.json"))
        class P:
            pass


def test_plugin_relative_manifest_resolves_from_caller(tmp_path: Path, monkeypatch) -> None:
    # Build a fake plugin module on disk that imports the SDK and decorates.
    pkg = tmp_path / "fakepkg"
    pkg.mkdir()
    (pkg / "plugin.json").write_text(json.dumps({"version": "9.9.9", "capabilities": []}))
    (pkg / "__init__.py").write_text("")
    (pkg / "p.py").write_text(
        "from dr_plugin_sdk import plugin\n"
        "@plugin(manifest='plugin.json')\n"
        "class P: pass\n"
    )
    monkeypatch.syspath_prepend(str(tmp_path))
    import importlib

    mod = importlib.import_module("fakepkg.p")
    assert _version(mod.P) == "9.9.9"


def test_two_methods_same_rpc_raises() -> None:
    with pytest.raises(TypeError):

        @plugin()
        class _Bad:
            @importer.sync
            async def a(self, args, *, emit, host):
                return None

            @importer.sync
            async def b(self, args, *, emit, host):
                return None
