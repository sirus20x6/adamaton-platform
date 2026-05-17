#!/command/with-contenv sh
# Generate s6-rc user-bundle contents from $WORKER_QUEUES.
#
# WORKER_QUEUES is a comma-separated list of queue names; an empty
# value (or no env at all) means "every queue baked into this image
# variant." Each entry must match a directory under /etc/s6-overlay/s6-rc.d/
# (other than the 'user' meta-bundle), or the container refuses to start.
#
# We touch /etc/s6-overlay/s6-rc.d/user/contents.d/<queue> per entry;
# s6-rc resolves the user bundle into actual longruns at container
# init time and supervises one process per queue.

set -e

contents="/etc/s6-overlay/s6-rc.d/user/contents.d"
mkdir -p "$contents"

queues="${WORKER_QUEUES:-}"
if [ -z "$queues" ]; then
    # All compiled-in queues — every s6-rc.d/<name> that has a 'run'
    # file, minus the 'user' meta-bundle that wires them up.
    queues=$(find /etc/s6-overlay/s6-rc.d -mindepth 1 -maxdepth 1 -type d \
        ! -name user \
        -exec sh -c 'test -f "$1/run" && basename "$1"' _ {} \; \
        | tr '\n' ',' | sed 's/,$//')
fi

IFS=','
for q in $queues; do
    q=$(echo "$q" | tr -d ' ')
    [ -z "$q" ] && continue
    if [ ! -d "/etc/s6-overlay/s6-rc.d/$q" ]; then
        echo "adamaton-worker: WORKER_QUEUES references '$q' but this image variant was not built with that queue" >&2
        echo "adamaton-worker: compiled-in queues: $(find /etc/s6-overlay/s6-rc.d -mindepth 1 -maxdepth 1 -type d ! -name user -printf '%f ' 2>/dev/null)" >&2
        exit 1
    fi
    touch "$contents/$q"
done

echo "adamaton-worker: starting queues=$queues"
