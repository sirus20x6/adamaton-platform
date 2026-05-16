"""Entry point invoked by plugin-host's ``command`` array.

The host execs ``python -m dr_plugin_search_tavily``; the SDK takes over
from there, binding to ``$DR_PLUGIN_SOCK`` and serving until shutdown.
"""

from __future__ import annotations

from dr_plugin_sdk import main

from .plugin import Plugin

if __name__ == "__main__":
    main(Plugin)
