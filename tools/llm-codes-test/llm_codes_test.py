#!/usr/bin/env python3
"""Ask LLMs (through OpenRouter) which part of an operator's router names is a city code,
and score the answers. Scratch test for the learn-rdns-rules LLM step; nothing is written
to the database.

Input (from export.sql): router-names.txt (one PTR name per line), learned-rules.tsv.
Domains WITH a learned rule are controls (we know the answer); the others are the test.

  export OPENROUTER_TEST_API_KEY=...            # never pass it on the command line
  python3 llm_codes_test.py --models anthropic/claude-haiku-5.5,deepseek/deepseek-v4-flash
  python3 llm_codes_test.py --report       # summary again from the saved answers

Answers are saved in out/<model>.jsonl; a (model, domain) already there is not asked again,
so a rerun costs nothing and an interrupted run continues.
"""
import argparse, concurrent.futures as cf, json, os, random, re, sys, time, unicodedata, urllib.error, urllib.request, zipfile
from collections import Counter, defaultdict

# Copied from hatchet/rdnsrules/learn.go roleTokens: an LLM rule may still use them (scoped
# overrides can), they are only counted.
STOP = set("""ccr core edge gw bb agg bng bras pe cr ar br dr sr mx lag rtr swi sw router cs cor cbr ear er ir lns
olt ont dsl cmts ge gi te xe fa eth ae be po vlan lo border bdr csr dist ags cpe nas nat fw lb vpn mpls ospf bgp
ip ips pool sub isp net nets static dynamic dyn adsl vdsl ppp pppoe cable mobile mob res resi biz bus business inf
infra cust customer client host hosted user users home dial ftth fttx fiber fibre broadband gprs lte cgn cgnat
wifi wlan red rev reverse dns mail www gateway srv server cloud cdn vps dedi dedicated colo dc
al ak az ca co ct de fl ga hi id il in ia ks ky la me md ma mi mn ms mo mt ne nv nh nj nm ny nc nd oh ok or pa ri
sc sd tn tx ut vt va wa wv wi wy ab bc mb nb nl ns nt nu on qc sk yt""".split())

# Public suffixes with two labels that are common in router names. Rough; enough for a test.
TWO_LEVEL = set("""co.uk ac.uk org.uk net.uk com.au net.au org.au com.br net.br com.ar net.ar com.mx net.mx co.jp ne.jp
or.jp ad.jp co.kr ne.kr or.kr co.nz net.nz co.za net.za com.tr net.tr com.cn net.cn com.tw net.tw com.hk net.hk
co.id net.id co.in net.in com.sg net.sg com.my net.my co.th net.th com.ph net.ph com.vn net.vn com.pk net.pk
com.ua net.ua com.ru net.ru com.pl net.pl com.co net.co com.pe net.pe com.ec net.ec com.uy net.uy com.py net.py
com.bo net.bo com.ve net.ve co.il net.il com.eg net.eg com.sa net.sa co.ke co.tz co.ug com.ng net.ng""".split())

LABEL_OK = re.compile(r"^[a-z0-9-]+$")
RE2_UNSUPPORTED = re.compile(r"\(\?[=!<]|\\[1-9]|\(\?P=|\(\?>|\*\+|\+\+|\?\+")

PROMPT = """You get reverse DNS (PTR) names of internet routers of one operator, domain {domain}.
Find the part of the name that encodes the router's CITY (an airport code like "fra", a UN/LOCODE like
"defra", a CLLI like "nycmny", an abbreviation or a full city name). Answer ONLY with JSON:

{{"location": true,
  "regex": "a Go RE2 regex, case-insensitive, matching a whole name, with exactly ONE capture group: the city token",
  "codes": {{"<token as captured, lowercase>": {{"country": "ISO 3166-1 alpha-2", "city": "English city name"}}}},
  "comment": "one short sentence"}}

Rules:
- Give a code only when you are confident of the city; leave out tokens you cannot decode.
- Router roles and interface names (core, edge, gw, ae1, xe-0-0-0, ...) are not places.
- If the names hold no city at all, answer {{"location": false, "regex": "", "codes": {{}}, "comment": "..."}}.
- Decode every distinct city token you see in the names below, not only a few.

Names ({n} of {total}):
{names}"""


def registered_domain(host):
    labels = host.split(".")
    if len(labels) < 2:
        return ""
    if len(labels) >= 3 and ".".join(labels[-2:]) in TWO_LEVEL:
        return ".".join(labels[-3:])
    return ".".join(labels[-2:])


def fold(s):
    s = unicodedata.normalize("NFKD", s or "").encode("ascii", "ignore").decode().lower()
    return re.sub(r"[^a-z0-9]+", " ", s).strip()


def load_names(path):
    by_domain = defaultdict(set)
    skipped = 0
    with open(path) as f:
        for line in f:
            host = line.strip().lower().rstrip(".")
            if not host or re.fullmatch(r"[0-9.]+", host):
                continue
            d = registered_domain(host)
            if not d or host == d:
                continue
            if not all(LABEL_OK.match(l) for l in host.split(".")):
                skipped += 1
                continue
            by_domain[d].add(host)
    return {d: sorted(v) for d, v in by_domain.items()}, skipped


def load_rules(path):
    """domain -> token -> (country, city) of the learned rules (all ranks)."""
    rules = defaultdict(dict)
    if not os.path.exists(path):
        return rules
    with open(path) as f:
        for line in f:
            parts = line.rstrip("\n").split("\t")
            if len(parts) < 4:
                continue
            try:
                places = json.loads(parts[3].replace("\\\\", "\\"))
            except json.JSONDecodeError:
                continue
            for token, p in places.items():
                if isinstance(p, dict):
                    rules[parts[0]][token] = (p.get("country", ""), p.get("city", ""))
    return rules


def load_geonames(path):
    """(country, folded name) pairs of GeoNames cities (cities1000/15000 .txt or .zip)."""
    known = set()
    if not path:
        return None
    if path.endswith(".zip"):
        z = zipfile.ZipFile(path)
        lines = (l.decode("utf-8") for l in z.open(z.namelist()[0]))
    else:
        lines = open(path, encoding="utf-8")
    for line in lines:
        c = line.split("\t")
        if len(c) < 9:
            continue
        for n in [c[1], c[2]] + c[3].split(","):
            if n:
                known.add((c[8], fold(n)))
    return known


def ask(model, prompt, key, timeout):
    body = {"model": model, "temperature": 0, "max_tokens": 4000,
            "messages": [{"role": "user", "content": prompt}],
            "response_format": {"type": "json_object"}, "usage": {"include": True}}
    req = urllib.request.Request("https://openrouter.ai/api/v1/chat/completions", data=json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + key, "Content-Type": "application/json",
                                          "X-Title": "rdns-codes-test"})
    for attempt in range(4):
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                data = json.load(r)
            break
        except urllib.error.HTTPError as e:
            msg = e.read()[:300].decode("utf-8", "replace")
            if e.code in (429, 500, 502, 503, 504) and attempt < 3:
                time.sleep(5 * (attempt + 1) ** 2)
                continue
            return {"error": f"HTTP {e.code}: {msg}", "seconds": time.time() - t0}
        except Exception as e:  # timeouts, connection resets
            if attempt < 3:
                time.sleep(5)
                continue
            return {"error": type(e).__name__, "seconds": time.time() - t0}
    if "error" in data:
        return {"error": str(data["error"])[:300], "seconds": time.time() - t0}
    usage = data.get("usage") or {}
    text = (data.get("choices") or [{}])[0].get("message", {}).get("content") or ""
    return {"text": text, "seconds": round(time.time() - t0, 1), "cost": usage.get("cost") or 0,
            "in": usage.get("prompt_tokens"), "out": usage.get("completion_tokens")}


def parse(text):
    m = re.search(r"\{.*\}", text or "", re.S)
    if not m:
        return None
    try:
        return json.loads(m.group(0))
    except json.JSONDecodeError:
        return None


def check(answer, names, sample, truth, geonames):
    """Scores one answer. names: all names of the domain; sample: those sent."""
    r = {"json": answer is not None}
    if answer is None:
        return r
    codes = {str(k).lower(): v for k, v in (answer.get("codes") or {}).items() if isinstance(v, dict)}
    r["location"] = bool(answer.get("location"))
    r["codes"] = len(codes)
    r["bad_country"] = sum(1 for v in codes.values() if not re.fullmatch(r"[A-Z]{2}", str(v.get("country", ""))))
    r["stop_tokens"] = sorted(t for t in codes if t in STOP)
    r["distinct_cities"] = len({(v.get("country"), fold(v.get("city"))) for v in codes.values()})
    if geonames is not None:
        r["unknown_city"] = sorted(t for t, v in codes.items() if (v.get("country"), fold(v.get("city"))) not in geonames)
    pattern = answer.get("regex") or ""
    r["regex_ok"] = False
    if pattern and not RE2_UNSUPPORTED.search(pattern):
        try:
            rx = re.compile(pattern if pattern.startswith("(?i)") else "(?i)" + pattern)
            r["regex_ok"] = rx.groups == 1
        except re.error:
            pass
    if r["regex_ok"]:
        sent = set(sample)
        dec = held = held_dec = 0
        used = Counter()
        for n in names:
            m = rx.fullmatch(n)
            tok = m.group(1).lower() if m and m.group(1) else None
            ok = tok in codes
            dec += ok
            if ok:
                used[tok] += 1
            if n not in sent:
                held += 1
                held_dec += ok
        r["decoded_pct"] = round(100 * dec / len(names), 1)
        r["holdout_names"] = held
        r["holdout_decoded_pct"] = round(100 * held_dec / held, 1) if held else None
        r["used_tokens"] = len(used)
    r["passed"] = bool(r["regex_ok"] and r.get("used_tokens", 0) >= 2 and r["bad_country"] == 0
                       and r.get("decoded_pct", 0) >= 20)
    if truth:
        same = [t for t in codes if t in truth]
        r["truth_tokens"] = len(same)
        r["truth_country_ok"] = sum(1 for t in same if codes[t].get("country") == truth[t][0])
        r["truth_city_ok"] = sum(1 for t in same if fold(codes[t].get("city")) == fold(truth[t][1]))
    return r


def pick_domains(names, rules, n_test, n_control, min_names):
    big = sorted((d for d in names if len(names[d]) >= min_names), key=lambda d: (-len(names[d]), d))
    test = [d for d in big if d not in rules][:n_test]
    control = [d for d in big if d in rules][:n_control]
    return test, control


def out_path(model):
    return os.path.join("out", re.sub(r"[^A-Za-z0-9._-]+", "_", model) + ".jsonl")


def load_done(model):
    done = {}
    if os.path.exists(out_path(model)):
        with open(out_path(model)) as f:
            for line in f:
                row = json.loads(line)
                if "error" not in row:
                    done[row["domain"]] = row
    return done


def run_model(model, domains, names, rules, key, args, geonames):
    done = load_done(model)
    todo = [d for d in domains if d not in done]
    spent = sum(r.get("cost") or 0 for r in done.values())
    print(f"{model}: {len(done)} saved, {len(todo)} to ask", file=sys.stderr)
    os.makedirs("out", exist_ok=True)

    def one(d):
        rnd = random.Random(d)
        all_names = names[d]
        sample = sorted(rnd.sample(all_names, min(args.sample, len(all_names))))
        prompt = PROMPT.format(domain=d, n=len(sample), total=len(all_names), names="\n".join(sample))
        res = ask(model, prompt, key, args.timeout)
        row = {"domain": d, "control": d in rules, "names": len(all_names), **res}
        if "error" not in res:
            answer = parse(res["text"])
            row["answer"] = answer
            row["check"] = check(answer, all_names, sample, rules.get(d), geonames)
        return row

    with cf.ThreadPoolExecutor(args.parallel) as pool, open(out_path(model), "a") as f:
        futures = {}
        for d in todo:
            futures[pool.submit(one, d)] = d
        for fut in cf.as_completed(futures):
            row = fut.result()
            f.write(json.dumps(row) + "\n")
            f.flush()
            spent += row.get("cost") or 0
            c = row.get("check", {})
            status = row.get("error") or ("passed" if c.get("passed") else "failed")
            print(f"  {row['domain']:32s} {status:8.60s} {c.get('decoded_pct', '-')}% ${row.get('cost', 0):.4f}", file=sys.stderr)
            if spent > args.max_cost:
                print(f"{model}: budget ${args.max_cost} reached, stopping", file=sys.stderr)
                for other in futures:
                    other.cancel()
                break


def report(models, domains):
    rows = {m: load_done(m) for m in models}
    # Agreement: per (domain, token) the city most models gave; a model agrees when it gave it.
    votes = defaultdict(Counter)
    for m in models:
        for d, r in rows[m].items():
            for t, v in ((r.get("answer") or {}).get("codes") or {}).items():
                if isinstance(v, dict):
                    votes[(d, str(t).lower())][(v.get("country"), fold(v.get("city")))] += 1
    hdr = f"{'model':42s} {'asked':>5s} {'json':>4s} {'pass':>4s} {'dec%':>5s} {'hold%':>5s} {'ctrl city%':>10s} {'agree%':>6s} {'stop':>4s} {'nocity':>6s} {'$':>7s} {'s/req':>5s}"
    print(hdr)
    for m in models:
        rs = [r for d, r in rows[m].items() if d in domains]
        if not rs:
            continue
        ch = [r.get("check", {}) for r in rs]
        passed = [c for c in ch if c.get("passed")]
        dec = sum(c.get("decoded_pct", 0) for c in passed) / len(passed) if passed else 0
        hs = [c["holdout_decoded_pct"] for c in passed if c.get("holdout_decoded_pct") is not None]
        tt = sum(c.get("truth_tokens", 0) for c in ch)
        tc = sum(c.get("truth_city_ok", 0) for c in ch)
        agree = total = 0
        for r in rs:
            for t, v in ((r.get("answer") or {}).get("codes") or {}).items():
                if not isinstance(v, dict):
                    continue
                c = votes[(r["domain"], str(t).lower())]
                if sum(c.values()) < 2:
                    continue
                total += 1
                agree += c[(v.get("country"), fold(v.get("city")))] == max(c.values())
        nocity = sum(len(c.get("unknown_city", [])) for c in ch)
        print(f"{m:42.42s} {len(rs):5d} {sum(c.get('json', False) for c in ch):4d} {len(passed):4d} {dec:5.1f} "
              f"{(sum(hs) / len(hs) if hs else 0):5.1f} {(f'{100 * tc / tt:.0f} ({tt})' if tt else '-'):>10s} "
              f"{(100 * agree / total if total else 0):6.0f} {sum(len(c.get('stop_tokens', [])) for c in ch):4d} "
              f"{nocity:6d} {sum(r.get('cost') or 0 for r in rs):7.4f} {sum(r.get('seconds', 0) for r in rs) / len(rs):5.1f}")
    print("""
asked: domains answered (errors are not saved and are retried next run)   json: parsable answers
pass: regex compiles, 1 group, >=2 decoded tokens used, ISO countries, >=20% of the names decoded
dec%/hold%: names decoded among passed answers, all names / only names NOT sent to the model
ctrl city%: same city as our learned rule for the same token (control domains; tokens compared)
agree%: tokens where the model gave the city most models gave (tokens >=2 models decoded)
stop: stop-list tokens decoded   nocity: (country, city) not in GeoNames (only with --geonames)""")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--models", default="anthropic/claude-haiku-5.5,deepseek/deepseek-v4-flash,google/gemini-3.1-flash-lite,openai/gpt-6-luna,qwen/qwen3.7-flash,nvidia/nemotron-3-ultra-550b-a55b:free")
    p.add_argument("--names", default="router-names.txt")
    p.add_argument("--rules", default="learned-rules.tsv")
    p.add_argument("--geonames", help="cities1000.zip or cities15000.txt: check that each city exists")
    p.add_argument("--test", type=int, default=40, help="biggest domains without a learned rule")
    p.add_argument("--control", type=int, default=10, help="biggest domains with a learned rule")
    p.add_argument("--min-names", type=int, default=20)
    p.add_argument("--sample", type=int, default=60, help="names sent per domain")
    p.add_argument("--parallel", type=int, default=4)
    p.add_argument("--timeout", type=int, default=180)
    p.add_argument("--max-cost", type=float, default=1.0, help="USD per model per run")
    p.add_argument("--list", action="store_true", help="only print the chosen domains")
    p.add_argument("--report", action="store_true", help="only print the summary of saved answers")
    args = p.parse_args()
    models = [m.strip() for m in args.models.split(",") if m.strip()]

    names, skipped = load_names(args.names)
    rules = load_rules(args.rules)
    test, control = pick_domains(names, rules, args.test, args.control, args.min_names)
    domains = test + control
    if args.list:
        print(f"{sum(map(len, names.values()))} names, {len(names)} domains, {skipped} skipped (label outside [a-z0-9-]), {len(rules)} domains with rules")
        for d in domains:
            print(f"{'control' if d in rules else 'test   '} {len(names[d]):6d} {d}  e.g. {names[d][len(names[d]) // 2]}")
        return
    if not args.report:
        key = os.environ.get("OPENROUTER_TEST_API_KEY")
        if not key:
            sys.exit("set OPENROUTER_TEST_API_KEY")
        geonames = load_geonames(args.geonames)
        for m in models:
            run_model(m, domains, names, rules, key, args, geonames)
    report(models, set(domains))


if __name__ == "__main__":
    main()
