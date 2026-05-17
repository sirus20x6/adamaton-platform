#!/bin/sh
# Pre-init filter for WORKER_QUEUES.
#
# s6-overlay v3 compiles /etc/s6-overlay/s6-rc.d/ into the runtime db
# during /init — BEFORE any cont-init.d script runs. To filter which
# queue processes start, the user-bundle contents.d/ must already
# reflect the desired set at /init time. So this script runs FIRST
# (as the container's ENTRYPOINT), prunes contents.d/ entries that
# WORKER_QUEUES excludes, then execs the real s6-overlay /init.
#
# WORKER_QUEUES is a comma-separated subset of the queues compiled
# into this image variant. An empty value (or no env at all) means
# "every queue baked in" — the contents.d/ files installed by the
# Dockerfile are kept as-is. A non-empty value with a queue name that
# isn't in this variant is a hard error so misconfiguration fails fast
# rather than silently dropping work.

set -e

contents=/etc/s6-overlay/s6-rc.d/user/contents.d

if [ -d "$contents" ] && [ -n "${WORKER_QUEUES:-}" ]; then
    requested=$(echo "$WORKER_QUEUES" | tr ',' ' ')

    # Validate first — fail before touching anything if the operator
    # asked for a queue not compiled in.
    for r in $requested; do
        r=$(echo "$r" | tr -d ' ')
        [ -z "$r" ] && continue
        if [ ! -d "/etc/s6-overlay/s6-rc.d/$r" ]; then
            echo "adamaton-worker: WORKER_QUEUES references '$r' but this image variant was not built with that queue" >&2
            echo "adamaton-worker: compiled-in queues:" $(ls "$contents") >&2
            exit 1
        fi
    done

    # Prune any contents.d entry NOT in the requested set.
    for entry in "$contents"/*; do
        [ -f "$entry" ] || continue
        name=$(basename "$entry")
        keep=0
        for r in $requested; do
            r=$(echo "$r" | tr -d ' ')
            [ "$name" = "$r" ] && keep=1 && break
        done
        if [ "$keep" -eq 0 ]; then
            echo "adamaton-worker: pruning queue '$name' (not in WORKER_QUEUES=$WORKER_QUEUES)"
            rm -f "$entry"
        fi
    done
fi

echo "adamaton-worker: handing off to /init; final queue set:" $(ls "$contents" 2>/dev/null | tr '\n' ' ')
exec /init "$@"
