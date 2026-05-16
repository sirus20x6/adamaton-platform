#!/usr/bin/env bash
# Mirror a quota-relevant slice of the workstation's CCSAVER database
# to the Pi for the dashboard to read.
#
# The full CCSAVER DB is ~40 GB (every request body, response body, and
# headers blob for months of API traffic). The dashboard reader only
# needs the most recent rows per api_type + a few aggregates over the
# last 7 days, so we materialise a tiny snapshot that contains exactly
# the columns the reader queries.
#
# Run as a workstation systemd-user timer (see ccsaver-mirror.{service,timer})
# every minute. Atomic on the Pi via rsync into a temp file then mv.
#
# Env:
#   CCSAVER_SRC   - source DB (default: ~/.local/share/ccsaver/data.db)
#   CCSAVER_DEST  - Pi destination (default: deepresearch.local:/srv/ccsaver/data.db)
#   CCSAVER_DAYS  - days of history to mirror (default: 7)
#   CCSAVER_TMP   - local workdir (default: /tmp/ccsaver-mirror)

set -euo pipefail
umask 077

SRC="${CCSAVER_SRC:-$HOME/.local/share/ccsaver/data.db}"
DEST="${CCSAVER_DEST:-deepresearch.local:/srv/ccsaver/data.db}"
DAYS="${CCSAVER_DAYS:-7}"
TMP="${CCSAVER_TMP:-/tmp/ccsaver-mirror}"

# Validate CCSAVER_DAYS as a positive integer to prevent SQL injection
# via the heredoc below.
if ! [[ "$DAYS" =~ ^[1-9][0-9]*$ ]]; then
    echo "ccsaver-mirror: invalid CCSAVER_DAYS value '$DAYS' (must be a positive integer)" >&2
    exit 1
fi

REMOTE_HOST="${DEST%%:*}"
REMOTE_PATH="${DEST#*:}"

# Validate REMOTE_HOST: alnum, dot, underscore, dash, optional user@ prefix.
if ! [[ "$REMOTE_HOST" =~ ^[A-Za-z0-9._-]+(@[A-Za-z0-9._-]+)?$ ]]; then
    echo "ccsaver-mirror: invalid remote host '$REMOTE_HOST' in CCSAVER_DEST" >&2
    exit 1
fi

# Validate REMOTE_PATH: absolute path, no shell metacharacters.
if ! [[ "$REMOTE_PATH" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
    echo "ccsaver-mirror: invalid remote path '$REMOTE_PATH' in CCSAVER_DEST" >&2
    exit 1
fi

mkdir -p "$TMP"
chmod 700 "$TMP"
SNAP="$TMP/snapshot.db"

# Drop any stale snapshot.
rm -f "$SNAP" "$SNAP-shm" "$SNAP-wal"

# The reader (delegator/quota/ccsaver.go) only touches these columns.
# Build a fresh DB containing just those, with the same indexes the
# reader's predicates use. ATTACH + CREATE TABLE AS SELECT is the
# smallest way to produce a portable, single-file output.
sqlite3 "$SRC" <<SQL
ATTACH DATABASE '$SNAP' AS snap;
CREATE TABLE snap.interactions AS
  SELECT id, timestamp, api_type, model,
         response_headers, estimated_cost_usd,
         input_tokens, output_tokens, duration_ms
    FROM interactions
   WHERE timestamp >= datetime('now', '-${DAYS} days');
CREATE INDEX snap.idx_api_type   ON interactions(api_type);
CREATE INDEX snap.idx_timestamp  ON interactions(timestamp);
CREATE INDEX snap.idx_api_id     ON interactions(api_type, id);
DETACH DATABASE snap;
SQL

# Tighten perms on the snapshot before it leaves the host: the DB
# contains response_headers which may include sensitive material.
chmod 600 "$SNAP"

# Rsync the small file to a temp name on the Pi and rename atomically
# so the reader never sees a half-written DB. One SSH connection only.
# The single quotes inside the double-quoted ssh argument are sent to
# the remote shell as-is; combined with the REMOTE_PATH validator above
# this neutralises shell-metachar injection on the remote side.
rsync -a --inplace "$SNAP" "${REMOTE_HOST}:${REMOTE_PATH}.tmp"
# shellcheck disable=SC2029  # client-side expansion is intentional; REMOTE_PATH is validated above
ssh "$REMOTE_HOST" "mv '${REMOTE_PATH}.tmp' '${REMOTE_PATH}'"
