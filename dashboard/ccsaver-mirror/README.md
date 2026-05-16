# ccsaver-mirror

Workstation-side timer that ships a quota-relevant slice of the CCSAVER
SQLite database to the Pi every minute so the dashboard's delegator
panel can read real rate-limit + cost numbers without exposing the full
40 GB capture file or opening a port back to the workstation.

## How it works

```
workstation              Pi
============              ==
~/.local/share/ccsaver/   /srv/ccsaver/
  data.db (~40 GB)          data.db (~5-20 MB, last 7d slice)
       │                          ▲
       │ sqlite3 ATTACH+SELECT   bind-mount :ro
       ▼                          │
  /tmp/ccsaver-mirror/      docker compose → evo-api
       snapshot.db          CCSAVER_DB=/data/ccsaver/data.db
       │
       │ rsync + atomic mv
       ▼
   Pi:/srv/ccsaver/data.db.tmp → mv → data.db
```

`mirror.sh` builds a fresh SQLite file containing only the columns the
reader queries (`response_headers`, `estimated_cost_usd`, `timestamp`,
`api_type`, `model`, `*_tokens`, `duration_ms`) for the last N days
(default 7), rsyncs it to the Pi under a `.tmp` name, then renames in
place so the reader never observes a partial DB.

## Install (workstation)

```bash
# 1. Make sure the script is reachable from the unit file's ExecStart.
chmod +x /thearray/git/evo/dashboard/ccsaver-mirror/mirror.sh

# 2. Link service + timer into the user systemd path.
mkdir -p ~/.config/systemd/user
ln -sf /thearray/git/evo/dashboard/ccsaver-mirror/ccsaver-mirror.service \
       ~/.config/systemd/user/ccsaver-mirror.service
ln -sf /thearray/git/evo/dashboard/ccsaver-mirror/ccsaver-mirror.timer \
       ~/.config/systemd/user/ccsaver-mirror.timer

# 3. Enable + start.
systemctl --user daemon-reload
systemctl --user enable --now ccsaver-mirror.timer

# 4. Verify.
systemctl --user list-timers ccsaver-mirror.timer
journalctl --user -u ccsaver-mirror.service -n 30
```

## Pi-side setup

Once per Pi:

```bash
ssh deepresearch.local 'sudo install -d -o sirus -g sirus /srv/ccsaver'
```

In `/home/sirus/deepresearch/docker-compose.yml`, the `evo-api` service
gets a read-only bind-mount and an env var pointing at the mirror:

```yaml
evo-api:
  # …existing…
  volumes:
    - /srv/ccsaver:/data/ccsaver:ro
  environment:
    # …existing…
    CCSAVER_DB: /data/ccsaver/data.db
```

Restart with `docker compose up -d evo-api` and the
`/api/v1/delegator/quota` endpoint will return real utilization values
within ~60 s of the first mirror run.

## Tuning

| env var        | default                                  | notes                                  |
|----------------|------------------------------------------|----------------------------------------|
| `CCSAVER_SRC`  | `$HOME/.local/share/ccsaver/data.db`     | source DB                              |
| `CCSAVER_DEST` | `deepresearch.local:/srv/ccsaver/data.db`| `host:path` for rsync                   |
| `CCSAVER_DAYS` | `7`                                      | only rows from the last N days are kept |
| `CCSAVER_TMP`  | `/tmp/ccsaver-mirror`                    | local scratch                          |

Override via `Environment=` lines in the `.service` file if you need a
non-default `CCSAVER_DEST` (e.g. a different host) or want a smaller
window.

## Failure modes

- **Pi unreachable** → systemd marks the service as failed; the next
  tick retries. No data loss; the previous snapshot stays on the Pi.
- **sqlite3 locked** → the workstation ccsaver uses WAL, so concurrent
  reads are fine. The script's read-only `ATTACH` won't block the
  proxy's writes.
- **rsync mid-rename** → `mv` is atomic on the same filesystem; the
  reader either sees the old DB or the new one.
- **Workstation DB vacuumed** → the script re-derives the slice from
  scratch each tick, so vacuums or row deletions just shrink the next
  snapshot.
