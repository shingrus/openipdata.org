# hostingping

Run by hand about once a month in Google Cloud Shell. It pings the `.1` of every /24 announced by a
hosting network (about 2.2M addresses) once from each of 27 Google Cloud regions, and writes one file,
`hosting-rtt.csv.gz`: header `prefix` plus one column per region, named by its site (`US/Ashburn`),
one row per /24 with the round-trip time in ms (empty = no answer), only /24s that at least 3 regions
answered. The file goes into the worker VM's inbox `/var/lib/openipdata/ping/inbox/` and is imported
with the workflow `import-hosting-ping`.

Standard library only (Python 3, bash, gcloud). Nothing here reads or writes anything outside this folder
and the working directory.

## Cost and time

- About $1.5 to $2 a run, paid by the owner: about 60M pings, about 5 GB egress, 27 e2-micro VMs for
  about an hour each.
- About 40 minutes of pinging per region. All 27 regions start at once (`PAR=27`, the default), so a run takes
  about an hour; a region the project's quota refuses ("VM not created") is started by the next run.
- Each VM shuffles its own copy of the target list, so the 27 pings of an address are spread over the run.

## Run

1. Create a Google Cloud project with billing and open Cloud Shell in it.
2. Upload this folder (or clone the repo) and `cd` into it.
3. `bash run.sh` (`PAR=6 bash run.sh` for fewer regions at a time). The VMs keep pinging when Cloud Shell
   closes; run it again later to collect the results, delete the VMs and start what is left.

It downloads the public `asn-openipdata.sqlite3`, makes `work/targets.txt` with `targets.py`, uploads it to
the month's bucket `gs://<project>-hosting-ping-<YYYYMM>` (UTC), starts one VM per region, deletes each VM when
its result is in the bucket, and merges the results with `merge.py`. It can simply be run again: regions with
a result are skipped, a VM that is still pinging (Cloud Shell disconnected) is waited for, and the target list
in the bucket is reused so all regions ping the same addresses. A run cut off at the end of a month is resumed
with `RUN=<YYYYMM> bash run.sh`. The end of the run prints the next steps: download `hosting-rtt.csv.gz` from
Cloud Shell's menu, `scp` it to the worker inbox, start `import-hosting-ping` in Hatchet, delete the bucket
(the exact command is printed).

The .1 of a /24 is pinged whatever it is, so a RIPE Atlas probe that sits at a .1 is pinged too; the scores
of such probes come from their own address, which makes the first production score a little kinder than the
test (which left probe addresses out).

## Opt-out list

`exclude.txt`: one IPv4 prefix per line, `#` comments allowed. Any /24 that overlaps a listed prefix is not
pinged. A line that is not a prefix stops `targets.py` with its line number.

## Files

- `regions.tsv`: region and site (`<country>/<city>`, a GeoNames name) per line; the column order of the output.
- `targets.py`: target list from the SQLite file (networks of category `hosting`); refuses fewer than
  100,000 or more than 6,000,000 targets.
- `merge.py`: streams the sorted region results into the CSV.
- `run.sh`: the Cloud Shell driver; it prints the progress of all regions every 5 minutes.
- `status.sh`: the progress of each region of this month's run at any time, also when `run.sh` is not
  running (`done`, the VM's last step such as `pinging, chunk 3 of 12 done`, `VM starting`, `not started`).

## Tests

From this folder: `python3 -I -B test_hostingping.py`. The tests set `HOSTINGPING_MIN_TARGETS` and
`HOSTINGPING_MAX_TARGETS` to accept tiny data; never set them in a real run.
