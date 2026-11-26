"""Make the ping target list: the .1 of every /24 announced by a hosting network.

usage: python3 -I targets.py <asn-openipdata.sqlite3> <exclude.txt> <targets.txt>

Test only: HOSTINGPING_MIN_TARGETS and HOSTINGPING_MAX_TARGETS replace the limits
(100,000 and 6,000,000). Never set them in a real run.
"""
import bisect
import ipaddress
import json
import os
import random
import sqlite3
import sys

MIN_TARGETS = int(os.environ.get("HOSTINGPING_MIN_TARGETS", "100000"))
MAX_TARGETS = int(os.environ.get("HOSTINGPING_MAX_TARGETS", "6000000"))
SEED = 1


def read_exclude(path):
    """Return sorted, merged /24-index intervals (first, last) of the exclude prefixes."""
    spans = []
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            text = line.split("#", 1)[0].strip()
            if not text:
                continue
            try:
                net = ipaddress.IPv4Network(text, strict=False)
            except ValueError:
                sys.exit("exclude.txt: line %d is not an IPv4 prefix" % n)
            spans.append((int(net.network_address) >> 8, int(net.broadcast_address) >> 8))
    spans.sort()
    merged = []
    for a, b in spans:
        if merged and a <= merged[-1][1] + 1:
            merged[-1] = (merged[-1][0], max(merged[-1][1], b))
        else:
            merged.append((a, b))
    return merged


def excluded(merged, starts, idx):
    i = bisect.bisect_right(starts, idx) - 1
    return i >= 0 and idx <= merged[i][1]


def is_public(idx):
    a = ipaddress.IPv4Address((idx << 8) | 1)
    return a.is_global and not (a.is_multicast or a.is_reserved or a.is_loopback or a.is_link_local)


def main():
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    db_path, exclude_path, out_path = sys.argv[1:]
    merged = read_exclude(exclude_path)
    starts = [a for a, _ in merged]

    con = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)
    networks = 0
    prefixes = 0
    blocks = set()
    for (text,) in con.execute("SELECT json FROM asn"):
        try:
            doc = json.loads(text)
            if doc["metadata"]["category"] != "hosting":
                continue
            plist = doc["prefixes"]["ipv4"]
        except (TypeError, ValueError, KeyError):
            continue
        if not isinstance(plist, list):
            continue
        networks += 1
        for p in plist:
            if not isinstance(p, str):
                continue
            try:
                net = ipaddress.IPv4Network(p, strict=False)
            except ValueError:
                continue
            if net.prefixlen < 8:
                continue
            first = int(net.network_address) >> 8
            count = 1 if net.prefixlen >= 24 else 1 << (24 - net.prefixlen)
            prefixes += 1
            blocks.update(range(first, first + count))
    con.close()

    public = [i for i in blocks if is_public(i)]
    kept = [i for i in public if not excluded(merged, starts, i)]
    kept.sort()
    random.Random(SEED).shuffle(kept)
    print("hosting networks: %d" % networks)
    print("prefixes: %d" % prefixes)
    print("/24s: %d" % len(public))
    print("excluded: %d" % (len(public) - len(kept)))
    print("targets: %d" % len(kept))
    if not MIN_TARGETS <= len(kept) <= MAX_TARGETS:
        sys.exit("refusing: %d targets is outside %d to %d" % (len(kept), MIN_TARGETS, MAX_TARGETS))
    tmp = out_path + ".tmp"
    with open(tmp, "w", encoding="ascii") as f:
        for i in kept:
            f.write(str(ipaddress.IPv4Address((i << 8) | 1)) + "\n")
    os.replace(tmp, out_path)


if __name__ == "__main__":
    main()
