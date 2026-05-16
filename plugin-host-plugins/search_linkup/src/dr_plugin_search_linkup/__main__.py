"""Entry point invoked by plugin-host's ``command`` array."""

from __future__ import annotations

from dr_plugin_sdk import main

from .plugin import Plugin

if __name__ == "__main__":
    main(Plugin)
