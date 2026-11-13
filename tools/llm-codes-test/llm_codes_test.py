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
import argparse, concurrent.futures as cf, json, os, random, re, sys, threading, time, unicodedata, urllib.error, urllib.request, zipfile
from collections import Counter, defaultdict

_log_lock = threading.Lock()
LOG_FILE = os.path.join("out", "run.log")


def log(msg):
    """Prints a timestamped line to stderr and appends it to out/run.log."""
    line = time.strftime("%H:%M:%S ") + msg
    with _log_lock:
        print(line, file=sys.stderr, flush=True)
        os.makedirs("out", exist_ok=True)
        with open(LOG_FILE, "a") as f:
            f.write(line + "\n")

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


def same_city(a, b):
    """Same city when the folded names are equal or one starts the other ("frankfurt" / "frankfurt am main")."""
    a, b = fold(a), fold(b)
    return bool(a) and bool(b) and (a == b or a.startswith(b + " ") or b.startswith(a + " "))


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
                if isinstance(p, list) and len(p) >= 2:  # stored as ["JP", "Gifu"]
                    rules[parts[0]][token] = (p[0], p[1])
                elif isinstance(p, dict):
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


def ask(model, prompt, key, timeout, label="", max_tokens=16000, reasoning="low"):
    body = {"model": model, "temperature": 0, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}],
            "response_format": {"type": "json_object"}, "usage": {"include": True}}
    # Thinking models spend output tokens on reasoning before the answer; "none" turns it off
    # where the model allows, "default" leaves the model's own setting.
    if reasoning == "none":
        body["reasoning"] = {"enabled": False}
    elif reasoning != "default":
        body["reasoning"] = {"effort": reasoning}
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
                wait = 5 * (attempt + 1) ** 2
                log(f"  {label}: HTTP {e.code} after {time.time() - t0:.1f}s, retry {attempt + 1}/3 in {wait}s: {msg}")
                time.sleep(wait)
                continue
            return {"error": f"HTTP {e.code}: {msg}", "seconds": round(time.time() - t0, 1)}
        except Exception as e:  # timeouts, connection resets
            if attempt < 3:
                log(f"  {label}: {type(e).__name__} after {time.time() - t0:.1f}s, retry {attempt + 1}/3 in 5s: {e}")
                time.sleep(5)
                continue
            return {"error": f"{type(e).__name__}: {e}", "seconds": round(time.time() - t0, 1)}
    if "error" in data:
        return {"error": str(data["error"])[:300], "seconds": round(time.time() - t0, 1)}
    usage = data.get("usage") or {}
    choice = (data.get("choices") or [{}])[0]
    text = choice.get("message", {}).get("content") or ""
    if not text.strip():
        rt = (usage.get("completion_tokens_details") or {}).get("reasoning_tokens")
        return {"error": f"empty answer (finish={choice.get('finish_reason')}, {usage.get('completion_tokens')} output tokens, "
                         f"{rt} of them reasoning)",
                "seconds": round(time.time() - t0, 1), "cost": usage.get("cost") or 0}
    return {"text": text, "seconds": round(time.time() - t0, 1), "cost": usage.get("cost") or 0,
            "in": usage.get("prompt_tokens"), "out": usage.get("completion_tokens"),
            "finish": choice.get("finish_reason"), "provider": data.get("provider"),
            "reasoning_tokens": ((usage.get("completion_tokens_details") or {}).get("reasoning_tokens")),
            "reasoning_chars": len(choice.get("message", {}).get("reasoning") or "")}


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
        r["passed"], r["fail_reasons"] = False, ["answer is not JSON"]
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
    if pattern and RE2_UNSUPPORTED.search(pattern):
        r["regex_error"] = "uses a construct RE2 has not (lookaround, backreference, possessive)"
    elif pattern:
        try:
            rx = re.compile(pattern if pattern.startswith("(?i)") else "(?i)" + pattern)
            r["regex_ok"] = rx.groups == 1
            if rx.groups != 1:
                r["regex_error"] = f"{rx.groups} capture groups"
        except re.error as e:
            r["regex_error"] = f"does not compile: {e}"
    if r["regex_ok"]:
        sent = set(sample)
        dec = held = held_dec = 0
        used, unknown = Counter(), Counter()
        examples, unmatched = [], []
        for n in names:
            m = rx.fullmatch(n)
            tok = m.group(1).lower() if m and m.group(1) else None
            ok = tok in codes
            dec += ok
            if ok:
                used[tok] += 1
                if len(examples) < 5 and used[tok] == 1:
                    examples.append(f"{n} -> {tok} -> {codes[tok].get('country')} {codes[tok].get('city')}")
            elif tok:
                unknown[tok] += 1
            elif len(unmatched) < 5:
                unmatched.append(n)
            if n not in sent:
                held += 1
                held_dec += ok
        r["decoded_pct"] = round(100 * dec / len(names), 1)
        r["holdout_names"] = held
        r["holdout_decoded_pct"] = round(100 * held_dec / held, 1) if held else None
        r["used_tokens"] = len(used)
        r["unused_codes"] = sorted(t for t in codes if t not in used)
        r["matched_no_code"] = sum(unknown.values())
        r["top_captured_no_code"] = [f"{t}x{c}" for t, c in unknown.most_common(10)]
        r["no_match_names"] = sum(1 for n in names if not rx.fullmatch(n))
        r["examples"] = examples
        r["no_match_examples"] = unmatched
    reasons = []
    if not pattern:
        reasons.append("no regex")
    elif not r["regex_ok"]:
        reasons.append("regex does not compile, is not RE2 or has not exactly 1 group")
    if r["regex_ok"] and r.get("used_tokens", 0) < 2:
        reasons.append(f"only {r.get('used_tokens', 0)} decoded token(s) used in the names")
    if r["bad_country"]:
        reasons.append(f"{r['bad_country']} country code(s) not ISO alpha-2")
    if r["regex_ok"] and r.get("decoded_pct", 0) < 20:
        reasons.append(f"decodes only {r.get('decoded_pct', 0)}% of the names")
    r["fail_reasons"] = reasons
    r["passed"] = not reasons
    if truth:
        same = [t for t in codes if t in truth]
        r["truth_tokens"] = len(same)
        r["truth_country_ok"] = sum(1 for t in same if codes[t].get("country") == truth[t][0])
        r["truth_city_ok"] = sum(1 for t in same if same_city(codes[t].get("city"), truth[t][1]))
        r["truth_mismatches"] = [f"{t}: model {codes[t].get('country')} {codes[t].get('city')} / ours {truth[t][0]} {truth[t][1]}"
                                 for t in same if not same_city(codes[t].get("city"), truth[t][1])][:10]
        r["truth_missed"] = len([t for t in truth if t not in codes])
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


def fmt_secs(x):
    x = int(x)
    return f"{x // 3600}h{x % 3600 // 60:02d}m" if x >= 3600 else f"{x // 60}m{x % 60:02d}s"


def run_model(model, domains, names, rules, key, args, geonames, progress):
    done = load_done(model)
    todo = [d for d in domains if d not in done]
    spent = sum(r.get("cost") or 0 for r in done.values())
    log(f"=== {model}: {len(done)} domains already answered (${spent:.4f}, not asked again), "
        f"{len(todo)} to ask, {args.parallel} in parallel, budget ${args.max_cost}")
    os.makedirs("out", exist_ok=True)
    t_model = time.time()

    def one(d):
        rnd = random.Random(d)
        all_names = names[d]
        sample = sorted(rnd.sample(all_names, min(args.sample, len(all_names))))
        prompt = PROMPT.format(domain=d, n=len(sample), total=len(all_names), names="\n".join(sample))
        kind = "control" if d in rules else "test"
        log(f"-> {model} {d} ({kind}): sending {len(sample)} of {len(all_names)} names, {len(prompt)} chars")
        res = ask(model, prompt, key, args.timeout, label=f"{model} {d}", max_tokens=args.max_tokens, reasoning=args.reasoning)
        row = {"domain": d, "control": d in rules, "names": len(all_names), **res}
        if "error" not in res:
            answer = parse(res["text"])
            row["answer"] = answer
            row["check"] = check(answer, all_names, sample, rules.get(d), geonames)
        return row

    def show(row):
        d, c, a = row["domain"], row.get("check", {}), row.get("answer") or {}
        if "error" in row:
            log(f"<- {model} {d}: ERROR after {row.get('seconds')}s: {row['error']} (not saved, asked again next run)")
            return
        log(f"<- {model} {d}: {row['seconds']}s, {row.get('in')} in / {row.get('out')} out tokens, ${row.get('cost') or 0:.5f}, "
            f"of them reasoning {row.get('reasoning_tokens')} tokens ({row.get('reasoning_chars')} chars), "
            f"finish={row.get('finish')}, provider={row.get('provider')}")
        if row.get("finish") == "length":
            log(f"   CUT OFF: the reply hit --max-tokens {args.max_tokens}"
                + (" while still reasoning (no answer text); try --reasoning none or a higher --max-tokens"
                   if not row.get("text") else "; the answer may be incomplete"))
        if not c.get("json"):
            log(f"   not JSON, raw answer starts: {row.get('text', '')[:400]!r}")
        else:
            log(f"   location={a.get('location')} regex={a.get('regex')!r}")
            if a.get("comment"):
                log(f"   comment: {str(a['comment'])[:300]}")
            if c.get("regex_error"):
                log(f"   regex: {c['regex_error']}")
            log(f"   codes {c.get('codes')} ({c.get('distinct_cities')} distinct cities), used in names {c.get('used_tokens', '-')}, "
                f"names decoded {c.get('decoded_pct', '-')}% (held-out: {str(c['holdout_decoded_pct']) + '% of ' + str(c['holdout_names']) if c.get('holdout_decoded_pct') is not None else 'none, all names were sent'})")
            if "no_match_names" in c:
                log(f"   regex misses {c['no_match_names']} names; {c['matched_no_code']} matched with a token that has no code")
            for e in c.get("examples", []):
                log(f"   ok    {e}")
            for n in c.get("no_match_examples", []):
                log(f"   miss  {n}")
            if c.get("top_captured_no_code"):
                log(f"   captured but not decoded: {', '.join(c['top_captured_no_code'])}")
            if c.get("unused_codes"):
                log(f"   codes never captured by the regex: {', '.join(c['unused_codes'][:20])}")
            if c.get("stop_tokens"):
                log(f"   stop-list tokens decoded: {', '.join(c['stop_tokens'])}")
            if c.get("unknown_city"):
                log(f"   cities not in GeoNames: {', '.join(c['unknown_city'][:20])}")
            if "truth_tokens" in c:
                log(f"   control: {c['truth_tokens']} tokens shared with our rule, city right {c['truth_city_ok']}, "
                    f"country right {c['truth_country_ok']}, our tokens the model left out {c['truth_missed']}")
                for mm in c.get("truth_mismatches", []):
                    log(f"   differs {mm}")
        log(f"   {'PASS' if c.get('passed') else 'FAIL: ' + '; '.join(c.get('fail_reasons', []))}")

    with cf.ThreadPoolExecutor(args.parallel) as pool, open(out_path(model), "a") as f:
        futures = {}
        for d in todo:
            futures[pool.submit(one, d)] = d
        n = 0
        for fut in cf.as_completed(futures):
            row = fut.result()
            n += 1
            if "error" not in row:
                f.write(json.dumps(row) + "\n")
                f.flush()
            spent += row.get("cost") or 0
            show(row)
            progress["done"] += 1
            elapsed = time.time() - progress["start"]
            left = progress["total"] - progress["done"]
            eta = elapsed / progress["done"] * left
            log(f"   STATUS model {progress['model']}/{progress['models']} {model}: {n}/{len(todo)} done, {len(todo) - n} left, "
                f"${spent:.4f} of ${args.max_cost} | ALL: {progress['done']}/{progress['total']} done, {left} left, "
                f"{fmt_secs(elapsed)} so far, about {fmt_secs(eta)} to go")
            if spent > args.max_cost:
                log(f"{model}: budget ${args.max_cost} reached, cancelling the {len(todo) - n} domains not asked yet")
                for other in futures:
                    other.cancel()
                progress["total"] -= len(todo) - n
                break
    log(f"=== {model}: done in {time.time() - t_model:.0f}s, ${spent:.4f} in total")


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
    p.add_argument("--max-tokens", type=int, default=16000, help="output tokens per reply, reasoning included")
    p.add_argument("--reasoning", default="low", choices=["none", "low", "medium", "high", "default"],
                   help="reasoning effort for thinking models (none = off where the model allows)")
    p.add_argument("--max-cost", type=float, default=1.0, help="USD per model per run")
    p.add_argument("--list", action="store_true", help="only print the chosen domains")
    p.add_argument("--report", action="store_true", help="only print the summary of saved answers")
    args = p.parse_args()
    models = [m.strip() for m in args.models.split(",") if m.strip()]

    log(f"start: models {', '.join(models)}; max {args.max_tokens} output tokens, reasoning {args.reasoning}, "
        f"{args.parallel} requests in parallel (-> and <- lines of different domains interleave; match them by domain)")
    names, skipped = load_names(args.names)
    log(f"{args.names}: {sum(map(len, names.values()))} router names in {len(names)} domains, "
        f"{skipped} names skipped (a label outside [a-z0-9-])")
    rules = load_rules(args.rules)
    log(f"{args.rules}: {len(rules)} domains with a learned rule, {sum(map(len, rules.values()))} tokens")
    test, control = pick_domains(names, rules, args.test, args.control, args.min_names)
    domains = test + control
    big = sum(1 for d in names if len(names[d]) >= args.min_names)
    log(f"{big} domains have at least {args.min_names} names; picked {len(test)} test (no rule) and {len(control)} control (with a rule)")
    for d in domains:
        log(f"  {'control' if d in rules else 'test   '} {len(names[d]):6d} names  {d}  e.g. {names[d][len(names[d]) // 2]}")
    if args.list:
        return
    if not args.report:
        key = os.environ.get("OPENROUTER_TEST_API_KEY")
        if not key:
            sys.exit("set OPENROUTER_TEST_API_KEY")
        geonames = load_geonames(args.geonames)
        log(f"GeoNames: {len(geonames)} (country, name) pairs from {args.geonames}" if geonames is not None
            else "GeoNames: not given, cities are not checked (--geonames)")
        log(f"answers go to out/<model>.jsonl, this log to {LOG_FILE}")
        todo = {m: sum(1 for d in domains if d not in load_done(m)) for m in models}
        progress = {"done": 0, "total": sum(todo.values()), "start": time.time(), "models": len(models), "model": 0}
        log(f"plan: {progress['total']} requests in all: " + ", ".join(f"{m} {todo[m]}" for m in models)
            + " (saved answers are not asked again)")
        for m in models:
            progress["model"] += 1
            run_model(m, domains, names, rules, key, args, geonames, progress)
    report(models, set(domains))


if __name__ == "__main__":
    main()
