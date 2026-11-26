"""Tests for targets.py and merge.py. Run: python3 -I -B test_hostingping.py"""
import csv
import gzip
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
# The count limits are replaced by these two variables only here, so tiny test data is accepted.
TEST_ENV = dict(os.environ, HOSTINGPING_MIN_TARGETS="1", HOSTINGPING_MAX_TARGETS="1000")


def run(script, *args, env=None):
    return subprocess.run([sys.executable, "-I", os.path.join(HERE, script), *args],
                          capture_output=True, text=True, env=env)


class TargetsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = self.tmp.name
        self.db = os.path.join(self.dir, "asn.sqlite3")
        self.out = os.path.join(self.dir, "targets.txt")
        self.exc = os.path.join(self.dir, "exclude.txt")
        with open(self.exc, "w") as f:
            f.write("# nothing\n")

    def tearDown(self):
        self.tmp.cleanup()

    def make_db(self, rows):
        con = sqlite3.connect(self.db)
        con.execute("CREATE TABLE asn (asn INTEGER PRIMARY KEY, json TEXT)")
        for i, j in enumerate(rows, 1):
            con.execute("INSERT INTO asn VALUES (?, ?)", (i, j if isinstance(j, str) else json.dumps(j)))
        con.commit()
        con.close()

    @staticmethod
    def doc(cat, prefixes):
        return {"metadata": {"category": cat}, "prefixes": {"ipv4": prefixes}}

    def targets(self):
        with open(self.out) as f:
            return f.read().split()

    def test_selection(self):
        self.make_db([
            self.doc("hosting", ["8.8.0.0/22", "9.9.9.128/25", "10.0.0.0/8", "11.0.0.0/7", "bad", 5]),
            self.doc("isp", ["20.1.0.0/24"]),
            "not json",
            self.doc("hosting", "not a list"),
            {"metadata": "x"},
            self.doc("hosting", ["8.8.0.0/22", "100.64.0.0/24", "224.1.1.0/24"]),
        ])
        p = run("targets.py", self.db, self.exc, self.out, env=TEST_ENV)
        self.assertEqual(p.returncode, 0, p.stderr)
        got = set(self.targets())
        self.assertTrue({"8.8.0.1", "8.8.1.1", "8.8.2.1", "8.8.3.1"} <= got)
        self.assertIn("9.9.9.1", got)
        self.assertNotIn("20.1.0.1", got)
        self.assertFalse(any(a.startswith("11.") for a in got))
        self.assertFalse(any(a.startswith("10.") for a in got))  # not public
        self.assertNotIn("100.64.0.1", got)
        self.assertNotIn("224.1.1.1", got)
        self.assertEqual(len(self.targets()), len(got))  # no duplicates
        self.assertEqual(len(got), 5)

    def test_exclude(self):
        self.make_db([self.doc("hosting", ["8.8.0.0/22"])])
        with open(self.exc, "w") as f:
            f.write("# comment\n\n8.8.1.0/24  # trailing\n8.8.2.0/23\n")
        p = run("targets.py", self.db, self.exc, self.out, env=TEST_ENV)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.targets(), ["8.8.0.1"])
        self.assertIn("excluded: 3", p.stdout)

    def test_bad_exclude_line(self):
        self.make_db([self.doc("hosting", ["8.8.0.0/22"])])
        with open(self.exc, "w") as f:
            f.write("# ok\n\nsecret-text\n")
        p = run("targets.py", self.db, self.exc, self.out, env=TEST_ENV)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("line 3", p.stderr)
        self.assertNotIn("secret-text", p.stderr)

    def test_limits(self):
        self.make_db([self.doc("hosting", ["8.8.0.0/22"])])
        p = run("targets.py", self.db, self.exc, self.out)  # real limits
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(os.path.exists(self.out))

    def test_shuffle_is_fixed(self):
        self.make_db([self.doc("hosting", ["8.8.0.0/16"])])
        run("targets.py", self.db, self.exc, self.out, env=dict(TEST_ENV, HOSTINGPING_MAX_TARGETS="100000"))
        first = self.targets()
        self.assertEqual(len(first), 256)
        self.assertNotEqual(first, sorted(first, key=lambda a: tuple(map(int, a.split(".")))))
        os.remove(self.out)
        run("targets.py", self.db, self.exc, self.out, env=dict(TEST_ENV, HOSTINGPING_MAX_TARGETS="100000"))
        self.assertEqual(first, self.targets())


class MergeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = self.tmp.name
        self.regions = os.path.join(self.dir, "regions.tsv")
        with open(self.regions, "w", encoding="utf-8") as f:
            f.write("r1\tUS/One\nr2\tCA/Montréal\nr3\tDE/Three\nr4\tFR/Four\n")
        self.out = os.path.join(self.dir, "out.csv.gz")

    def tearDown(self):
        self.tmp.cleanup()

    def result(self, region, lines):
        with gzip.open(os.path.join(self.dir, region + ".txt.gz"), "wt") as f:
            f.write("".join(l + "\n" for l in lines))

    def rows(self):
        with gzip.open(self.out, "rt", encoding="utf-8", newline="") as f:
            return list(csv.reader(f))

    def test_merge(self):
        self.result("r1", ["1.1.1.1 : 12.34", "1.1.2.1 : 5.0", "1.1.3.1 : -", "1.1.4.1 : 0.01",
                           "1.1.4.1 : 0.5", "garbage line", "1.1.5.1 : 1.0 (duplicate)", "1.1.10.1 : 3.0"])
        self.result("r2", ["1.1.1.1 : 20.0", "1.1.2.1 : 6.0", "1.1.3.1 : 7.0", "1.1.4.1 : 2.0", "1.1.10.1 : 4.0"])
        self.result("r3", ["1.1.1.1 : 30.0", "1.1.2.1 : -", "1.1.3.1 : 8.0", "1.1.4.1 : 3.0", "1.1.10.1 : -"])
        # r4 has no file: its column is empty and a warning is printed
        p = run("merge.py", self.dir, self.regions, self.out)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("warning: no result file for r4", p.stdout)
        rows = self.rows()
        self.assertEqual(rows[0], ["prefix", "US/One", "CA/Montréal", "DE/Three", "FR/Four"])
        self.assertEqual(rows[1], ["1.1.1.0/24", "12.3", "20.0", "30.0", ""])
        # 1.1.2: only two answers, dropped; 1.1.3: r2 and r3 only, dropped
        # 1.1.4: r1 repeated, keeps the lowest 0.01 which shows as 0.1
        self.assertEqual(rows[2], ["1.1.4.0/24", "0.1", "2.0", "3.0", ""])
        self.assertEqual(len(rows), 3)
        self.assertIn("rows written: 2", p.stdout)
        self.assertIn("rows with fewer than 3 answers: 3", p.stdout)
        self.assertIn("ignored lines: 2", p.stdout)

    def test_numeric_order(self):
        for r in ("r1", "r2", "r3"):
            self.result(r, ["2.0.0.1 : 1.0", "10.0.0.1 : 1.0", "100.0.0.1 : 1.0"])
        p = run("merge.py", self.dir, self.regions, self.out)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual([r[0] for r in self.rows()[1:]], ["2.0.0.0/24", "10.0.0.0/24", "100.0.0.0/24"])


if __name__ == "__main__":
    unittest.main()
