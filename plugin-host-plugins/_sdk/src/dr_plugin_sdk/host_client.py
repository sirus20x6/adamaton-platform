"""Plugin -> host gRPC client.

The host serves the dr.plugin.v1.Host service on a Unix socket whose
path lives in DR_HOST_SOCK. Every dedup / row / staged-path call the
plugin needs goes through here.
"""

from __future__ import annotations

import os
from typing import Any

import grpc
from google.protobuf import struct_pb2
from google.protobuf.json_format import MessageToDict, ParseDict

from . import _pb

_host = _pb.host_pb2
_host_grpc = _pb.host_pb2_grpc


_LEVELS = {
    "debug": _host.EmitLogRequest.LEVEL_DEBUG,
    "info": _host.EmitLogRequest.LEVEL_INFO,
    "warn": _host.EmitLogRequest.LEVEL_WARN,
    "warning": _host.EmitLogRequest.LEVEL_WARN,
    "error": _host.EmitLogRequest.LEVEL_ERROR,
}

_KINDS = {
    "counter": _host.EmitMetricRequest.KIND_COUNTER,
    "gauge": _host.EmitMetricRequest.KIND_GAUGE,
    "histogram": _host.EmitMetricRequest.KIND_HISTOGRAM,
}


def _struct(d: dict[str, Any] | None) -> struct_pb2.Struct:
    s = struct_pb2.Struct()
    if d:
        ParseDict(d, s)
    return s


class HostClient:
    """Async wrapper over the generated HostStub.

    Translates dicts <-> protobuf Struct at the boundary so plugin code
    never imports protobuf directly.
    """

    def __init__(self, channel: grpc.aio.Channel) -> None:
        self._chan = channel
        self._stub = _host_grpc.HostStub(channel)

    async def close(self) -> None:
        await self._chan.close()

    async def __aenter__(self) -> "HostClient":
        return self

    async def __aexit__(self, *_exc: Any) -> None:
        await self.close()

    # ----- Dedup ------------------------------------------------------

    async def is_known(self, plugin_id: str, external_id: str) -> tuple[bool, str]:
        resp = await self._stub.IsKnown(
            _host.IsKnownRequest(plugin_id=plugin_id, external_id=external_id)
        )
        return resp.known, resp.document_id

    # ----- Importer rows ----------------------------------------------

    async def upsert_import_row(
        self, run_id: str, plugin_id: str, table: str, row: dict[str, Any]
    ) -> str:
        resp = await self._stub.UpsertImportRow(
            _host.UpsertImportRowRequest(
                run_id=run_id,
                plugin_id=plugin_id,
                table=table,
                row=_struct(row),
            )
        )
        return resp.id

    # ----- File staging -----------------------------------------------

    async def stage_path(self, run_id: str, filename: str, content_type: str = "") -> str:
        resp = await self._stub.StagePath(
            _host.StagePathRequest(
                run_id=run_id, filename=filename, content_type=content_type
            )
        )
        return resp.path

    async def write_attachment(
        self, run_id: str, filename: str, content_type: str, body: bytes
    ) -> str:
        resp = await self._stub.WriteAttachment(
            _host.WriteAttachmentRequest(
                run_id=run_id,
                filename=filename,
                content_type=content_type,
                body=body,
            )
        )
        return resp.path

    # ----- Config ------------------------------------------------------

    async def get_config(self) -> dict[str, Any]:
        resp = await self._stub.GetConfig(_host.GetConfigRequest())
        return MessageToDict(resp.config) if len(resp.config.fields) else {}

    async def set_config(self, cfg: dict[str, Any]) -> None:
        await self._stub.SetConfig(_host.SetConfigRequest(config=_struct(cfg)))

    # ----- Observability ----------------------------------------------

    async def emit_metric(
        self,
        kind: str,
        name: str,
        value: float,
        labels: dict[str, str] | None = None,
    ) -> None:
        k = _KINDS.get(kind.lower())
        if k is None:
            raise ValueError(f"unknown metric kind: {kind!r}")
        await self._stub.EmitMetric(
            _host.EmitMetricRequest(kind=k, name=name, value=value, labels=labels or {})
        )

    async def emit_log(self, level: str, message: str, **fields: Any) -> None:
        lv = _LEVELS.get(level.lower())
        if lv is None:
            raise ValueError(f"unknown log level: {level!r}")
        await self._stub.EmitLog(
            _host.EmitLogRequest(level=lv, message=message, fields=_struct(fields))
        )


def make_host_client(sock: str | None = None) -> HostClient:
    """Open a channel to the host's Unix socket.

    Falls back to DR_HOST_SOCK env var when no path is passed — that's
    how the plugin-host launches plugins in production.
    """
    sock = sock or os.environ.get("DR_HOST_SOCK")
    if not sock:
        raise RuntimeError("DR_HOST_SOCK is not set; cannot reach host")
    chan = grpc.aio.insecure_channel(f"unix://{sock}")
    return HostClient(chan)


__all__ = ["HostClient", "make_host_client"]
