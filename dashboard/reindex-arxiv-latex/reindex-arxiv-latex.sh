#!/usr/bin/env bash
# Re-ingest every arxiv-tagged R2R document whose current source_tier
# is not 'latex'. Hardened v2: state lives in /home/sirus (survives
# tmpfs wipes on reboot), per-paper "in-flight" marker so post-crash
# we can identify the killer paper, longer inter-paper sleep to give
# the Pi headroom.

set -u
BASE="https://deepresearch.local/platform/sources/ingest"
DELETE_URL="https://deepresearch.local/v3/documents"
STATE_FILE="/home/sirus/reindex-state.jsonl"
INFLIGHT_FILE="/home/sirus/reindex-inflight.txt"
LOG_FILE="/home/sirus/reindex.log"
QUEUE_FILE="/home/sirus/reindex-queue.jsonl"

# On startup, if there's a left-over inflight marker the previous
# run died mid-paper. Record it as a poison entry so we skip it.
if [ -s "$INFLIGHT_FILE" ]; then
  prev=$(cat "$INFLIGHT_FILE")
  echo "{\"arxiv\":\"$prev\",\"status\":\"poison_skip\",\"reason\":\"crashed_in_flight\"}" >> "$STATE_FILE"
  > "$INFLIGHT_FILE"
fi

exec > >(tee -a "$LOG_FILE") 2>&1
echo "=== reindex start: $(date -u +%FT%TZ) ==="

PG_EXEC=(docker exec deepresearch-postgres-1 psql -U postgres -d postgres)

# Build queue. Also exclude papers we've previously marked poison.
POISON=""
if [ -f "$STATE_FILE" ]; then
  POISON=$(grep -E '"poison_skip"' "$STATE_FILE" | \
    python3 -c 'import sys,json; ids=set()
[ids.add(json.loads(l)["arxiv"]) for l in sys.stdin if l.strip()]
print(",".join(repr(x) for x in ids))' 2>/dev/null)
fi
POISON_FILTER=""
if [ -n "$POISON" ]; then
  POISON_FILTER="AND d.metadata->>'arxiv_id' NOT IN ($POISON)"
fi

"${PG_EXEC[@]}" -tA -c "
  SELECT row_to_json(t) FROM (
    SELECT d.id::text AS old_doc_id,
           d.metadata->>'arxiv_id' AS arxiv_id,
           COALESCE(d.title, d.metadata->>'arxiv_id', '?') AS title
    FROM deepresearch.documents d
    WHERE d.metadata ? 'arxiv_id'
      AND (
        COALESCE(d.metadata->>'source_tier', '') <> 'latex'
        OR d.summary IS NULL OR d.summary = ''
      )
      $POISON_FILTER
    ORDER BY d.metadata->>'arxiv_id'
  ) t;
" > "$QUEUE_FILE"

total=$(wc -l < "$QUEUE_FILE")
echo "queue size: $total (poison-skipped: $(echo "$POISON" | grep -oP "'[^']+'" | wc -l))"

ok=0; skip=0; fail=0; i=0
start_epoch=$(date +%s)

while IFS= read -r row; do
  [ -z "$row" ] && continue
  i=$((i+1))

  read -r old_id axid title < <(
    printf '%s' "$row" | python3 -c '
import sys, json
d = json.load(sys.stdin)
print(d["old_doc_id"], d["arxiv_id"], d.get("title","?"))'
  )

  if [[ ! "$axid" =~ ^[A-Za-z\-\.]+/[0-9]{7}$ ]] && \
     [[ ! "$axid" =~ ^[0-9]{4}\.[0-9]{4,5}(v[0-9]+)?$ ]]; then
    printf '[%4d/%s] %-15s SKIP invalid id\n' "$i" "$total" "$axid"
    skip=$((skip+1))
    printf '{"i":%d,"arxiv":"%s","old":"%s","status":"skip_invalid"}\n' \
      "$i" "$axid" "$old_id" >> "$STATE_FILE"
    continue
  fi

  new_id=$(python3 -c 'import uuid; print(uuid.uuid4())')

  # Mark this paper as in-flight BEFORE doing anything expensive.
  # If the Pi dies during the next ingest, the next startup will
  # see this and add the paper to the poison list.
  echo "$axid" > "$INFLIGHT_FILE"

  printf '[%4d/%s] %-15s old=%.8s ' "$i" "$total" "$axid" "$old_id"

  resp=$(curl -sk -m 240 -X POST "$BASE" \
    -H "Content-Type: application/json" \
    -d "{\"target\":\"arxiv:$axid\",\"document_id\":\"$new_id\"}")

  ok_flag=$(printf '%s' "$resp" | python3 -c '
import sys, json
try:
    d = json.loads(sys.stdin.read())
    print("yes" if d.get("ok") else "no")
except Exception:
    print("no")' 2>/dev/null)

  tier=$(printf '%s' "$resp" | python3 -c '
import sys, json
try: print(json.load(sys.stdin).get("source_tier",""))
except: print("")' 2>/dev/null)

  if [ "$ok_flag" != "yes" ]; then
    detail=$(printf '%s' "$resp" | python3 -c '
import sys, json
raw = sys.stdin.read()
try: print(json.loads(raw).get("detail", raw[:200]))
except: print(raw[:200])' 2>/dev/null)
    printf 'FAIL %.80s\n' "$detail"
    fail=$((fail+1))
    safe=${detail//\'/\'\'}
    printf '{"i":%d,"arxiv":"%s","old":"%s","status":"fetch_fail","detail":"%s"}\n' \
      "$i" "$axid" "$old_id" "${safe:0:200}" >> "$STATE_FILE"
    > "$INFLIGHT_FILE"
    sleep 4
    continue
  fi

  if [ "$tier" != "latex" ]; then
    curl -sk -m 30 -X DELETE "$DELETE_URL/$new_id" >/dev/null 2>&1 || true
    printf 'SKIP tier=%s\n' "$tier"
    skip=$((skip+1))
    printf '{"i":%d,"arxiv":"%s","old":"%s","new":"%s","status":"no_latex","tier":"%s"}\n' \
      "$i" "$axid" "$old_id" "$new_id" "$tier" >> "$STATE_FILE"
    > "$INFLIGHT_FILE"
    sleep 4
    continue
  fi

  "${PG_EXEC[@]}" -q -c "
    UPDATE platform.corpus_documents SET document_id='$new_id' WHERE document_id='$old_id';
    DELETE FROM platform.document_figures WHERE document_id='$old_id';
  " >/dev/null 2>&1

  curl -sk -m 60 -X DELETE "$DELETE_URL/$old_id" >/dev/null 2>&1 || true

  figs=$(printf '%s' "$resp" | python3 -c '
import sys, json
try: print(json.load(sys.stdin).get("figure_count",0))
except: print(0)' 2>/dev/null)
  size=$(printf '%s' "$resp" | python3 -c '
import sys, json
try: print(json.load(sys.stdin).get("size",0))
except: print(0)' 2>/dev/null)

  printf 'OK new=%.8s figs=%s size=%s\n' "$new_id" "$figs" "$size"
  ok=$((ok+1))
  printf '{"i":%d,"arxiv":"%s","old":"%s","new":"%s","status":"swapped","tier":"%s","figs":%s,"size":%s}\n' \
    "$i" "$axid" "$old_id" "$new_id" "$tier" "$figs" "$size" >> "$STATE_FILE"

  # Clear inflight marker — this paper completed cleanly.
  > "$INFLIGHT_FILE"

  if [ $((i % 25)) -eq 0 ]; then
    now=$(date +%s); elapsed=$((now - start_epoch))
    rate=$(python3 -c "print(f'{$i/$elapsed*60:.1f}')" 2>/dev/null || echo "?")
    eta_s=$(python3 -c "print(int(($total - $i) * $elapsed / max($i,1)))" 2>/dev/null || echo 0)
    eta_h=$(python3 -c "print(f'{$eta_s/3600:.1f}')" 2>/dev/null || echo "?")
    echo "  ... progress: ok=$ok skip=$skip fail=$fail rate=${rate}/min eta=${eta_h}h"
    free -h | head -2
  fi

  # Longer rest — gives the Pi time to flush R2R's worker-lo queue
  # so memory pressure doesn't compound across iterations.
  sleep 5
done < "$QUEUE_FILE"

echo "=== reindex done: $(date -u +%FT%TZ) ==="
echo "totals: ok=$ok skip=$skip fail=$fail (of $total)"
