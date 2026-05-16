"""Read items from a user-uploaded ``zotero.sqlite`` file.

Users without an API key (or who don't want to share one) upload a
zipped Zotero data directory; the multipart endpoint extracts the
sqlite file (and optionally the ``storage/<itemKey>/<file>`` tree of
attachments) and hands the path here. We open the database read-only
because a still-running Zotero client may have a write lock.

Schema reference: https://www.zotero.org/support/dev/web_api/v3/file_upload
and the auto-updated docs at https://api.zotero.org/types . The minimum
joins we need are::

    items                 (itemID, itemTypeID, key, dateAdded)
    itemDataValues        (valueID, value)
    itemData              (itemID, fieldID, valueID)
    fields                (fieldID, fieldName)
    creators              (creatorID, firstName, lastName, fieldMode)
    itemCreators          (itemID, creatorID, creatorTypeID, orderIndex)
    creatorTypes          (creatorTypeID, creatorType)
    itemAttachments       (itemID, parentItemID, contentType, path)

We mirror the Web API's JSON envelope so the rest of the pipeline
(:func:`app.zotero.normalize.normalize_item`) doesn't have to know which
path the data took.
"""

from __future__ import annotations

import logging
import sqlite3
from collections.abc import Iterator
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


# Map from Zotero's internal field name to the Web API casing. The
# stored field name is camelCase already; the API capitalises a couple
# of identifier-like fields (``DOI``, ``ISBN``). Mirroring the API casing
# here keeps :mod:`app.zotero.normalize` blind to which path produced
# the dict.
_FIELD_KEY_OVERRIDES: dict[str, str] = {
    "DOI": "DOI",
    "ISBN": "ISBN",
    "ISSN": "ISSN",
    "url": "url",
    "title": "title",
    "publicationTitle": "publicationTitle",
    "date": "date",
    "extra": "extra",
    "abstractNote": "abstractNote",
    "callNumber": "callNumber",
    "archiveID": "archiveID",
}


def read_items(sqlite_path: Path) -> Iterator[dict[str, Any]]:
    """Yield Zotero JSON envelopes from a zotero.sqlite file.

    The function streams the cursor — we don't load the whole library
    into memory just to turn around and yield it. Items with a parent
    (i.e. attachments and notes) are skipped at the top level; their
    parent items reference them through the ``has_pdf`` flag we stamp
    onto ``data`` for downstream consumption.

    The connection is opened with ``mode=ro`` and ``immutable=1`` so the
    reader can race a still-running Zotero client without taking a
    write lock or being affected by an in-flight transaction.
    """

    sqlite_path = Path(sqlite_path)
    if not sqlite_path.exists():
        raise FileNotFoundError(f"sqlite file not found: {sqlite_path}")

    uri = f"file:{sqlite_path}?mode=ro&immutable=1"
    conn = sqlite3.connect(uri, uri=True)
    conn.row_factory = sqlite3.Row
    try:
        # Pre-build a couple of lookup tables (small, bounded, faster
        # than a 6-way join per item).
        item_types = _id_lookup(conn, "SELECT itemTypeID, typeName FROM itemTypes")
        creator_types = _id_lookup(
            conn, "SELECT creatorTypeID, creatorType FROM creatorTypes"
        )

        item_attachment_index = _build_attachment_index(conn)

        items_sql = """
            SELECT
                i.itemID AS item_id,
                i.itemTypeID AS item_type_id,
                i.key AS item_key,
                i.version AS item_version,
                i.dateAdded AS date_added
            FROM items AS i
            LEFT JOIN deletedItems AS d ON d.itemID = i.itemID
            WHERE d.itemID IS NULL
              AND i.itemTypeID NOT IN (
                  SELECT itemTypeID FROM itemTypes
                  WHERE typeName IN ('attachment','note','annotation')
              )
            ORDER BY i.dateAdded ASC
        """

        for row in conn.execute(items_sql):
            item_id = row["item_id"]
            try:
                envelope = _build_envelope(
                    conn,
                    item_id=int(item_id),
                    item_key=row["item_key"],
                    item_version=row["item_version"],
                    item_type_name=item_types.get(row["item_type_id"], ""),
                    creator_types=creator_types,
                    attachment_index=item_attachment_index,
                )
            except Exception:  # noqa: BLE001 - one bad item shouldn't kill the iter
                logger.exception(
                    "zotero sqlite: skipping unreadable item id=%s key=%s",
                    item_id,
                    row["item_key"],
                )
                continue
            yield envelope
    finally:
        conn.close()


def _build_envelope(
    conn: sqlite3.Connection,
    *,
    item_id: int,
    item_key: str,
    item_version: Any,
    item_type_name: str,
    creator_types: dict[int, str],
    attachment_index: dict[int, list[dict[str, Any]]],
) -> dict[str, Any]:
    data: dict[str, Any] = {
        "key": item_key,
        "version": int(item_version) if isinstance(item_version, int) else 0,
        "itemType": item_type_name,
    }

    # Field values
    field_sql = """
        SELECT f.fieldName AS field_name, idv.value AS value
        FROM itemData AS idd
        JOIN fields AS f ON f.fieldID = idd.fieldID
        JOIN itemDataValues AS idv ON idv.valueID = idd.valueID
        WHERE idd.itemID = ?
    """
    for row in conn.execute(field_sql, (item_id,)):
        field_name = row["field_name"]
        api_key = _FIELD_KEY_OVERRIDES.get(field_name, field_name)
        data[api_key] = row["value"]

    # Creators
    creators_sql = """
        SELECT c.firstName AS first_name,
               c.lastName  AS last_name,
               c.fieldMode AS field_mode,
               ic.creatorTypeID AS creator_type_id,
               ic.orderIndex AS order_index
        FROM itemCreators AS ic
        JOIN creators AS c ON c.creatorID = ic.creatorID
        WHERE ic.itemID = ?
        ORDER BY ic.orderIndex ASC
    """
    creators: list[dict[str, Any]] = []
    for row in conn.execute(creators_sql, (item_id,)):
        creator_type = creator_types.get(row["creator_type_id"], "author")
        # ``fieldMode = 1`` means the creator is single-name (institution).
        if row["field_mode"] == 1:
            creators.append(
                {
                    "creatorType": creator_type,
                    "name": (row["last_name"] or "").strip()
                    or (row["first_name"] or "").strip(),
                }
            )
        else:
            creators.append(
                {
                    "creatorType": creator_type,
                    "firstName": (row["first_name"] or "").strip(),
                    "lastName": (row["last_name"] or "").strip(),
                }
            )
    data["creators"] = creators

    # PDF attachments
    pdf_attachments = attachment_index.get(item_id, [])
    if pdf_attachments:
        data["has_pdf"] = True
        # Surface the first PDF's relative path so the sync orchestrator
        # can resolve it against the uploaded ``storage/`` tree.
        data["_pdf_attachments"] = pdf_attachments

    envelope: dict[str, Any] = {
        "key": item_key,
        "version": data.get("version", 0),
        "data": data,
        "library": {
            "type": "user",  # we don't know without auth context
            "id": 0,
        },
    }
    return envelope


def _id_lookup(conn: sqlite3.Connection, sql: str) -> dict[int, str]:
    out: dict[int, str] = {}
    for row in conn.execute(sql):
        out[int(row[0])] = str(row[1])
    return out


def _build_attachment_index(
    conn: sqlite3.Connection,
) -> dict[int, list[dict[str, Any]]]:
    """Map ``parentItemID -> [pdf attachment metadata, ...]``.

    Each entry includes the attachment's Zotero key (so callers can
    locate the file at ``storage/<itemKey>/<filename>``) and the stored
    relative path.
    """

    sql = """
        SELECT
            ia.parentItemID AS parent_id,
            i.key AS attachment_key,
            ia.contentType AS content_type,
            ia.path AS path
        FROM itemAttachments AS ia
        JOIN items AS i ON i.itemID = ia.itemID
        LEFT JOIN deletedItems AS d ON d.itemID = ia.itemID
        WHERE d.itemID IS NULL
          AND ia.parentItemID IS NOT NULL
          AND ia.contentType = 'application/pdf'
    """
    out: dict[int, list[dict[str, Any]]] = {}
    for row in conn.execute(sql):
        parent_id = row["parent_id"]
        if parent_id is None:
            continue
        out.setdefault(int(parent_id), []).append(
            {
                "key": row["attachment_key"],
                "contentType": row["content_type"],
                "path": row["path"],
            }
        )
    return out


__all__ = ["read_items"]
