#!/usr/bin/env bash
# modelgw 基线压测：只做「可复现的一次记录」，不设硬门槛。
# 用法：GATEWAY=http://127.0.0.1:8080 TOKEN=mg-xxx MODEL=gpt-5 bash scripts/bench.sh
set -euo pipefail

GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
TOKEN="${TOKEN:-}"
MODEL="${MODEL:-gpt-5}"
CONCURRENCY="${CONCURRENCY:-4}"
DURATION="${DURATION:-10}"
OUT="${OUT:-data/bench/$(date +%Y%m%d-%H%M%S).txt}"

mkdir -p "$(dirname "$OUT")"
: > "$OUT"

if [[ -z "$TOKEN" ]]; then
  echo "缺少 TOKEN（虚拟 key，mg- 开头）" >&2
  exit 2
fi

payload="{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"bench\"}],\"stream\":false}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

worker() {
  local id="$1" deadline=$(( SECONDS + DURATION )) ok=0 fail=0
  : > "$tmp/w$id"
  while (( SECONDS < deadline )); do
    local started elapsed code
    started=$(date +%s%3N)
    code=$(curl -sS -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
      -X POST "${GATEWAY}/v1/chat/completions" -d "$payload" || echo 000)
    elapsed=$(( $(date +%s%3N) - started ))
    echo "$elapsed" >> "$tmp/w$id"
    if [[ "$code" == 2* ]]; then ok=$((ok+1)); else fail=$((fail+1)); fi
  done
  echo "$ok $fail" > "$tmp/s$id"
}

for i in $(seq 1 "$CONCURRENCY"); do worker "$i" & done
wait

ok=0; fail=0
for f in "$tmp"/s*; do read -r o x < "$f"; ok=$((ok+o)); fail=$((fail+x)); done
total=$((ok+fail))

cat "$tmp"/w* | sort -n > "$tmp/all"
p95=$(awk '{a[NR]=$1} END{ if(NR==0){print 0} else {idx=int(NR*0.95); if(idx<1)idx=1; print a[idx]} }' "$tmp/all")
qps=$(awk -v t="$total" -v d="$DURATION" 'BEGIN{ if(d>0) printf "%.2f", t/d; else print "0" }')

{
  echo "time: $(date '+%Y-%m-%d %H:%M:%S')"
  echo "gateway: $GATEWAY  model: $MODEL  concurrency: $CONCURRENCY  duration: ${DURATION}s"
  echo "requests: $total  ok: $ok  fail: $fail"
  echo "qps: $qps  p95_ms: $p95"
} | tee -a "$OUT"
