#!/bin/bash
# Run in Google Cloud Shell:  bash run.sh
# Makes the target list from the published ASN data, pings the .1 of every /24 of the hosting networks
# once from one small VM per Google Cloud region, collects the answers in a test bucket, deletes each VM
# when it is done and merges everything into hosting-rtt.csv.gz in the current directory.
# Safe to run again: regions that already have a result are skipped, leftover VMs are deleted.
# PAR=<n> sets how many regions run at a time (default 27: all at once; a region the quota refuses
# is started by the next run); RUN=<YYYYMM> picks the month's bucket.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
PROJECT=$(gcloud config get-value project 2>/dev/null)
[ -n "$PROJECT" ] || { echo "no project: run  gcloud config set project <project id>"; exit 1; }
# One bucket per month, so a bucket that was not deleted is never taken for this month's run.
# A run cut off just before midnight UTC on the last day of a month: set RUN=<YYYYMM> to resume it.
RUN=${RUN:-$(date -u +%Y%m)}
BUCKET=gs://$PROJECT-hosting-ping-$RUN
PAR=${PAR:-27}
SQLITE_URL=https://github.com/shingrus/openipdata.org/releases/latest/download/asn-openipdata.sqlite3
REGIONS=$(grep -v '^#' "$DIR/regions.tsv" | cut -f1 | grep .)
export DIR BUCKET

echo "project $PROJECT, bucket $BUCKET, $PAR regions at a time"
gcloud services enable compute.googleapis.com storage.googleapis.com || exit 1
gcloud storage buckets describe "$BUCKET" >/dev/null 2>&1 ||
  gcloud storage buckets create "$BUCKET" --location=US --uniform-bucket-level-access || exit 1
SA=$(gcloud iam service-accounts list --filter="email~-compute@developer.gserviceaccount.com" --format="value(email)")
[ -n "$SA" ] || { echo "no default compute service account yet; wait a minute and run again"; exit 1; }
gcloud storage buckets add-iam-policy-binding "$BUCKET" --member="serviceAccount:$SA" \
  --role=roles/storage.objectAdmin >/dev/null || exit 1

mkdir -p work logs results
# One target list per bucket: a run that is repeated pings the same addresses as the regions already done.
if gcloud storage ls "$BUCKET/targets.txt" >/dev/null 2>&1; then
  # The VMs read it from the bucket; Cloud Shell does not need a copy.
  echo "using the target list already in the bucket"
else
  curl -fL -o work/asn.sqlite3.tmp "$SQLITE_URL" || { echo "download of the ASN data failed"; exit 1; }
  mv work/asn.sqlite3.tmp work/asn.sqlite3
  python3 -I "$DIR/targets.py" work/asn.sqlite3 "$DIR/exclude.txt" work/targets.txt || exit 1
  gcloud storage cp work/targets.txt "$BUCKET/targets.txt" || exit 1
fi

cat > work/startup.sh <<'EOF'
#!/bin/bash
md() { curl -s -H Metadata-Flavor:Google "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1"; }
R=$(md region)
BUCKET=$(md bucket)
# Progress for status.sh: one small object per region, overwritten at each step.
progress() { echo "$R $*" | gcloud storage cp - "$BUCKET/progress/$R.txt" >/dev/null 2>&1; }
progress "installing fping"
for _ in 1 2 3 4 5; do
  apt-get update -qq && apt-get install -y -qq fping && break
  sleep 30
done
# Without fping nothing is uploaded, so the region is not counted as done; run.sh deletes the VM
# after its wait and a new run tries the region again.
command -v fping >/dev/null || { progress "failed: fping did not install"; exit 1; }
mkdir -p /var/tmp/chunks
cd /var/tmp || exit 1
gcloud storage cp "$BUCKET/targets.txt" targets.txt
# Each VM pings in its own random order, so the 27 pings of an address (and a network's traffic)
# are spread over the run instead of arriving together. The answers are sorted below.
shuf targets.txt -o targets.txt
split -l 200000 targets.txt chunks/c.
: > raw.txt
n=$(find chunks -name 'c.*' | wc -l)
i=0
for c in chunks/c.*; do
  progress "pinging, chunk $i of $n done"
  fping -C 1 -q -r 0 -t 1000 -i 1 -f "$c" 2>> raw.txt
  i=$((i + 1))
done
progress "sorting and uploading"
grep -v duplicate raw.txt | LC_ALL=C sort -T /var/tmp -S 200M -t. -k1,1n -k2,2n -k3,3n -k4,4n | gzip > "$R.txt.gz"
gcloud storage cp "$R.txt.gz" "$BUCKET/out/$R.txt.gz"
progress "done"
EOF

one() {
  r=$1
  log=logs/$r.log
  old=$(gcloud compute instances list --filter="name=ping-$r" --format="value(zone.basename())" 2>/dev/null)
  if gcloud storage ls "$BUCKET/out/$r.txt.gz" >/dev/null 2>&1; then
    [ -n "$old" ] && gcloud compute instances delete "ping-$r" --zone="$old" --quiet >>"$log" 2>&1
    echo "$r: done before"; return
  fi
  if [ -n "$old" ]; then
    # A VM of a run that was cut off (Cloud Shell disconnected) is still pinging: wait for it.
    z=$old
    echo "$r: VM of an earlier run still in $z, waiting for it"
  else
    z=$(gcloud compute zones list --filter="name~^$r- AND status=UP" --format="value(name)" --limit=1)
    [ -n "$z" ] || { echo "$r: no zone"; return; }
    if ! gcloud compute instances create "ping-$r" --zone="$z" --machine-type=e2-micro \
        --image-family=debian-12 --image-project=debian-cloud --scopes=storage-rw \
        --metadata="region=$r,bucket=$BUCKET" --metadata-from-file=startup-script=work/startup.sh \
        --quiet >"$log" 2>&1; then
      echo "$r: VM not created (see $log)"; return
    fi
    echo "$r: VM in $z, pinging"
  fi
  for _ in $(seq 180); do
    sleep 60
    gcloud storage ls "$BUCKET/out/$r.txt.gz" >/dev/null 2>&1 && break
  done
  gcloud compute instances delete "ping-$r" --zone="$z" --quiet >>"$log" 2>&1
  if gcloud storage ls "$BUCKET/out/$r.txt.gz" >/dev/null 2>&1; then echo "$r: done, VM deleted"
  else echo "$r: no result after 3 hours, VM deleted"; fi
}
export -f one
# A summary of all regions every 5 minutes while the VMs work (the same as bash status.sh).
( while sleep 300; do echo; date -u +"%H:%M UTC"; RUN=$RUN bash "$DIR/status.sh"; done ) &
STATUS=$!
echo "$REGIONS" | xargs -P "$PAR" -I{} bash -c 'one {}'
kill "$STATUS" 2>/dev/null

rm -rf results
mkdir -p results
gcloud storage cp "$BUCKET/out/*.txt.gz" results/ >/dev/null 2>&1
echo
echo "regions with results: $(find results -name '*.txt.gz' | wc -l) of $(echo "$REGIONS" | wc -w)"
echo "VMs still running (should be none):"
gcloud compute instances list --filter="name~^ping-" --format="value(name,zone)"
echo
python3 -I "$DIR/merge.py" results "$DIR/regions.tsv" hosting-rtt.csv.gz || exit 1
echo
ls -l hosting-rtt.csv.gz
cat <<EOF

Next steps:
  1. Download hosting-rtt.csv.gz from Cloud Shell's menu (three dots > Download).
  2. Copy it to the worker VM:
       scp hosting-rtt.csv.gz <user>@<worker-vm>:/var/lib/openipdata/ping/inbox/
  3. Start the workflow import-hosting-ping in Hatchet.
  4. Delete the bucket:
       gcloud storage rm -r $BUCKET
EOF
