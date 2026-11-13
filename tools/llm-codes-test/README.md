# LLM city-code test

A scratch test, not run by the worker: which LLM best proposes city codes for router-name
domains that `learn-rdns-rules` cannot learn on its own. Each model gets 60 router names of a
domain and answers with a regex (one capture group: the city token) and a token → country/city
map. Nothing is written to the database. Only router hop names are sent, never visitor names.

## Run

From the repo root on the VM (`OPENROUTER_TEST_API_KEY` in `/etc/openipdata/worker.env`):

```bash
( while IFS= read -r l; do case $l in ''|\#*) continue;; esac; export "${l%%=*}=${l#*=}"; done < <(sudo cat /etc/openipdata/worker.env)
  cd tools/llm-codes-test && psql "$PGSQL" -f export.sql && python3 llm_codes_test.py --models openai/gpt-6.1-terra,openai/gpt-6-luna )
```

Answers are saved in `out/<model>.jsonl` (a domain already answered is not asked again), the
log in `out/run.log`. `--report` prints the table again; `--help` lists the options.

The 50 domains: the 40 biggest without a learned rule (test) and the 10 biggest with one
(control: the model's cities are compared with the rule's).

## Results (2026-11-13)

![Coverage vs. held-out quality](model-comparison.svg)

| Model | Usable answers (of 50) | Held-out names decoded | Control city match | Cost per usable answer |
| --- | --- | --- | --- | --- |
| openai/gpt-6.1-terra | 36 | 64.7% | 73% (85 tokens) | 0.61¢ |
| openai/gpt-6-luna | 28 | 66.8% | not measured | 0.19¢ |
| mistralai/mistral-small-2603 | 21 | 63.1% | 80% (35) | 0.87¢ |
| anthropic/claude-haiku-5.5 | 21 | 55.4% | not measured | 0.27¢ |
| minimax/minimax-m3.1-flash | 18 | 58.4% | 65% (68) | 0.14¢ |
| z-ai/glm-5.3-flash | 17 | 52.2% | 86% (70) | 0.20¢ |
| anthropic/claude-sonnet-5.5 | 14 | 63.8% | 88% (65) | 3.65¢ |
| deepseek/deepseek-v4-flash | 14 | 61.2% | not measured | 0.26¢ |
| openai/gpt-oss-120b | 14 | 60.0% | 79% (86) | 0.09¢ |
| google/gemini-3.1-flash-lite | 12 | 62.2% | not measured | 0.49¢ |

- **Usable:** the regex works and has one group, at least 2 decoded tokens occur in the names,
  ISO country codes, at least 20% of the names decoded. 11 of the 50 domains have no city in
  their names (all models agree: names like `syn-072-129-061-099.inf.spectrum.com`), so 39 is
  the ceiling. Terra reaches 36 of them.
- **Held-out:** names of the domain the model did not see, averaged over its usable answers.
- **Control city match** compares with a learned candidate rule, not ground truth, and counts
  different names of one place ("Dulles" / "Washington") as a mismatch. "Not measured": the
  control answers were saved before the control check worked.
- Not in the table: qwen/qwen3.8-flash (3 usable, 34 request errors), qwen/qwen3.7-flash and
  nvidia/nemotron-3-ultra-550b-a55b:free (1 usable each, mostly not JSON).

Next: control scores for every model, then the `propose-codes` task of `learn-rdns-rules`.
