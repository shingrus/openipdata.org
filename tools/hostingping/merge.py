"""Merge the per-region fping results into hosting-rtt.csv.gz.

usage: python3 -I merge.py <dir of region results> <regions.tsv> <hosting-rtt.csv.gz>

Each region result <region>.txt.gz has lines "a.b.c.1 : 12.3" or "a.b.c.1 : -",
sorted numerically by address. The files are streamed, never held in memory.
"""
import csv
import gzip
import heapq
import itertools
import math
import os
import sys

MIN_ANSWERS = 3


def read_regions(path):
    out = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            if not line.strip() or line.startswith("#"):
                continue
            region, site = line.rstrip("\n").split("\t")
            out.append((region, site))
    return out


def parse_addr(text):
    parts = text.split(".")
    if len(parts) != 4:
        return None
    v = 0
    for p in parts:
        if not p.isascii() or not p.isdigit() or int(p) > 255:
            return None
        v = (v << 8) | int(p)
    return v


def region_lines(path, idx, stats):
    """Yield (address, region index, rtt) for each answered line, in file order."""
    last = -1
    with gzip.open(path, "rt", encoding="utf-8", errors="replace") as f:
        for line in f:
            if "duplicate" in line:
                stats["ignored"] += 1
                continue
            parts = line.split()
            if len(parts) != 3 or parts[1] != ":":
                stats["ignored"] += 1
                continue
            addr = parse_addr(parts[0])
            if addr is None or addr < last:
                stats["ignored"] += 1
                continue
            last = addr
            stats["addresses"] += 1
            if parts[2] == "-":
                continue
            try:
                rtt = float(parts[2])
            except ValueError:
                stats["ignored"] += 1
                continue
            if not math.isfinite(rtt) or rtt < 0:
                stats["ignored"] += 1
                continue
            stats["answers"][idx] += 1
            yield addr, idx, rtt


def fmt(v):
    return "%.1f" % max(v, 0.1)


def main():
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    results_dir, regions_path, out_path = sys.argv[1:]
    regions = read_regions(regions_path)
    stats = {"addresses": 0, "ignored": 0, "answers": [0] * len(regions)}
    streams = []
    for idx, (region, site) in enumerate(regions):
        path = os.path.join(results_dir, region + ".txt.gz")
        if os.path.exists(path):
            streams.append(region_lines(path, idx, stats))
        else:
            print("warning: no result file for %s (%s)" % (region, site))

    written = 0
    short = 0
    tmp = out_path + ".tmp"
    with gzip.open(tmp, "wt", encoding="utf-8", newline="") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(["prefix"] + [site for _, site in regions])
        merged = heapq.merge(*streams, key=lambda t: t[0])
        for block, group in itertools.groupby(merged, key=lambda t: t[0] >> 8):
            cells = [None] * len(regions)
            for _, idx, rtt in group:
                if cells[idx] is None or rtt < cells[idx]:
                    cells[idx] = rtt
            if sum(c is not None for c in cells) < MIN_ANSWERS:
                short += 1
                continue
            a = block << 8
            prefix = "%d.%d.%d.0/24" % (a >> 24, (a >> 16) & 255, (a >> 8) & 255)
            w.writerow([prefix] + ["" if c is None else fmt(c) for c in cells])
            written += 1
    os.replace(tmp, out_path)

    print("addresses: %d" % stats["addresses"])
    print("ignored lines: %d" % stats["ignored"])
    print("rows written: %d" % written)
    print("rows with fewer than %d answers: %d" % (MIN_ANSWERS, short))
    for idx, (region, site) in enumerate(regions):
        print("answers %s (%s): %d" % (region, site, stats["answers"][idx]))


if __name__ == "__main__":
    main()
