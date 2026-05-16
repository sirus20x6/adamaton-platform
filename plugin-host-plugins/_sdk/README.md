# dr-plugin-sdk

Python SDK for deepresearch plugins. Plugins subprocess into the Go
`plugin-host` and speak gRPC over Unix sockets in both directions.

```python
from dr_plugin_sdk import plugin, importer, search, main

@plugin(manifest="plugin.json")
class MyPlugin:
    @importer.sync
    async def sync(self, args, *, emit, host):
        emit.progress("starting", seen=0)
        emit.item(PluginItem(external_id="abc", title="hello"))

if __name__ == "__main__":
    main(MyPlugin)
```

See `tests/` for end-to-end examples.
