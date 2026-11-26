#!/bin/bash
# Run in Google Cloud Shell at any time, also while run.sh is not running:  bash status.sh
# Shows each region of this month's run: done, its VM's last progress step, starting, or not started.
# RUN=<YYYYMM> shows another month's run.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
PROJECT=$(gcloud config get-value project 2>/dev/null)
[ -n "$PROJECT" ] || { echo "no project: run  gcloud config set project <project id>"; exit 1; }
RUN=${RUN:-$(date -u +%Y%m)}
BUCKET=gs://$PROJECT-hosting-ping-$RUN

done_list=$(gcloud storage ls "$BUCKET/out/" 2>/dev/null)
progress=$(gcloud storage cat "$BUCKET/progress/*.txt" 2>/dev/null)
vms=$(gcloud compute instances list --filter="name~^ping-" --format="value(name)" 2>/dev/null)
total=0
finished=0
while read -r r _; do
  case "$r" in "" | \#*) continue ;; esac
  total=$((total + 1))
  if grep -q "/out/$r.txt.gz$" <<<"$done_list"; then
    finished=$((finished + 1))
    state="done"
  elif step=$(grep "^$r " <<<"$progress" | cut -d' ' -f2-) && [ -n "$step" ]; then
    state=$step
  elif grep -qx "ping-$r" <<<"$vms"; then
    state="VM starting"
  else
    state="not started"
  fi
  printf '  %-26s %s\n' "$r" "$state"
done <"$DIR/regions.tsv"
echo "  $finished of $total regions done"
