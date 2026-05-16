# reindex-arxiv-latex

Re-ingests arxiv documents currently in the deepresearch R2R index back
through the LaTeX-preferred path (see `platform/backend/app/search/
arxiv_fetcher.py::DEFAULT_PREFER`). Each document that succeeds gets a
new structured chunkset (abstract / prose / equation / figure / etc.)
plus a fresh document summary from the workstation vLLM.

## What's here

| File                              | Role                                                 |
|-----------------------------------|------------------------------------------------------|
| `reindex-arxiv-latex.sh`          | Worker script — queries the existing `deepresearch.documents` table, walks every arxiv-tagged row whose `source_tier` isn't `latex` (or whose `summary` is null), POSTs each through `/platform/sources/ingest` with a new pre-assigned document_id, and rewires `platform.corpus_documents` to point at the new R2R doc. |
| `reindex-arxiv-latex.service`     | systemd user unit. `Restart=on-failure`, 30 s backoff, 5-fail-in-10-min storm cap, exits cleanly on queue drain. |

The worker is **resumable**: its initial SQL filters out rows that
already match `source_tier='latex' AND summary IS NOT NULL`, so killing
and restarting picks up where it left off (at the cost of re-checking
which papers are still pending).

## Install on the Pi

```sh
# 1. Drop the script in place
install -m 0755 reindex-arxiv-latex.sh ~/reindex-arxiv-latex.sh

# 2. Drop the unit in place
mkdir -p ~/.config/systemd/user
install -m 0644 reindex-arxiv-latex.service ~/.config/systemd/user/

# 3. Linger so user-scoped units run at boot without a session
sudo loginctl enable-linger "$USER"

# 4. Enable + start
systemctl --user daemon-reload
systemctl --user enable --now reindex-arxiv-latex.service
```

## Standard ops

```sh
systemctl --user status  reindex-arxiv-latex          # health
systemctl --user stop    reindex-arxiv-latex          # pause
systemctl --user restart reindex-arxiv-latex          # kick
journalctl --user -u reindex-arxiv-latex -f           # live tail
tail -f ~/reindex.log                                  # per-paper log
cat ~/reindex-state.jsonl | wc -l                      # progress count
```

## State files

Written to `$HOME/` so they survive `/tmp` wipes:

- `reindex.log` — append-only per-paper progress lines (also where
  StandardOutput from the systemd unit lands).
- `reindex-state.jsonl` — one line per attempted paper, JSON-encoded.
  Used to compute the current count and detect poison entries.
- `reindex-inflight.txt` — the arxiv_id currently mid-ingest. On boot
  if this file is non-empty, the killer paper is added to the poison
  list so the loop doesn't re-attempt it forever.
- `reindex-queue.jsonl` — the working queue built at script start.

## When to NOT run this

- During a deepresearch-backend image rebuild (the script's POSTs will
  fail with 502 until the new image stabilises).
- Before the Pi has a healthy `deepresearch-postgres-1` container —
  the initial queue query will fail. systemd will retry every 30 s,
  so a few failed starts at boot are expected and harmless.
