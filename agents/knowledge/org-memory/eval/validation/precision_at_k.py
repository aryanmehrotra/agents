#!/usr/bin/env python3
"""precision@k over a committed golden query set — §1.7's Retrieve gate.

WHY THIS EXISTS AS A FILE. Every ranking number this project has quoted was produced by an ad-hoc
script that was never committed, which is how a validation report shipped an arithmetically
impossible row ("substring 16, word-boundary 0") that nobody could review. A retrieval system whose
claim is calibrated honesty cannot have an unreproducible quality metric. Same discipline as
`dedup_density.py`: the check is code, the queries are data, both live in the repo.

The methodology is the Cranfield paradigm (Cleverdon, 1967) reduced to what a small corpus can
honestly support: fixed queries, a stated relevance criterion, and a metric computed the same way
every time. It is deliberately NOT a claim about absolute retrieval quality — 18 queries cannot
support that. It is a REGRESSION gate: a change that drops this number has degraded ranking, and a
change that raises it should say by how much. Exits non-zero on any miss, so it can gate a build.

Relevance criteria are coarse substring checks: objective and reproducible, which is what a
regression gate needs. Graded judgements (and therefore nDCG) need human labelling this corpus does
not have. See evaluate() for why there are TWO criteria rather than one.

Defaults to the committed FIXTURE query set, which pairs with fixture_corpus.jsonl and is the same
set the portable Go gate (golden_test.go) runs. Point --queries at your own file to evaluate a
private corpus; keep that file out of git, because a query set is a description of your corpus.

An earlier version shipped queries written against a private corpus. Sanitising them for publication
silently decoupled them from the data they described, and five of eighteen "failures" turned out to
be that drift rather than a regression — a quality gate that can rot without anyone noticing is
worse than none. Fixture and queries now live together and move together.

Usage:
    python3 precision_at_k.py [--url http://localhost:8000] [--k 3] [--queries FILE]
"""

import argparse
import json
import os
import sys
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))


def load_queries(path):
    out = []
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if line and not line.startswith("#"):
                out.append(json.loads(line))
    return out


def recall(url, q, k):
    # top_k NARROWS only (the server clamps to its configured value). If the server is configured
    # BELOW k this measures precision@<config>, so main() checks and says so rather than mislabelling.
    params = urllib.parse.urlencode({"q": q, "top_k": str(k)})
    with urllib.request.urlopen(f"{url}/recall?{params}", timeout=90) as resp:
        return json.load(resp)["data"]


def set_config(url, key, value):
    body = json.dumps({"key": key, "value": value, "by": "precision_at_k"}).encode()
    req = urllib.request.Request(url + "/config", data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def evaluate(url, queries, k):
    """Returns (hits, total, misses). A miss reports what surfaced instead — the useful part.

    Two criteria, because retrieval fails in two different ways:

      must_contain     — the right answer must be in the top k. Catches "we missed it".
      must_not_contain — a specific WRONG answer must NOT be in the top k. Catches "we returned the
                         opposite", which a must_contain check cannot see at all: a negation query
                         answered with the reversed rule scores as a normal miss, indistinguishable
                         from the harmless case of finding nothing.

    That blind spot was real. An earlier pass scored these pairs by "did both polarities return the
    SAME decision id", which counted 1 of 3 broken. Checking for the opposite CONTENT instead shows
    3 of 3 — the other two returned different decisions carrying reversed advice, which is the more
    dangerous failure, not the milder one.

    For a negation query, ABSTAINING is a pass. "I have no rule about failing closed" is correct and
    honest; "here is the rule about failing open" is not.
    """
    hits, misses = 0, []

    for spec in queries:
        data = recall(url, spec["q"], k)
        blob = " ".join(
            (it["decision"]["what"] + " " + it["decision"].get("why", "")).lower()
            for it in data["items"]
        )

        want = spec.get("must_contain")
        forbid = spec.get("must_not_contain")

        ok = True
        reason = ""

        if want and want.lower() not in blob:
            ok, reason = False, f"missing {want!r}"

        if forbid and forbid.lower() in blob:
            ok, reason = False, f"returned the OPPOSITE — found {forbid!r}"

        if ok:
            hits += 1
        else:
            top = data["items"][0]["decision"]["what"] if data["items"] else "(abstained)"
            misses.append((spec["q"], reason, top))

    return hits, len(queries), misses


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://localhost:8000")
    ap.add_argument("--k", type=int, default=3)
    ap.add_argument("--queries", default=os.path.join(HERE, "golden_fixture_queries.jsonl"))
    ap.add_argument("--sweep", default=None, help="key=v1,v2,v3 — measure P@k at each value")
    args = ap.parse_args()

    queries = load_queries(args.queries)

    # Report the k actually in force. The previous version printed the k it ASKED for, which the
    # server ignored — a metric that silently mislabels itself is worse than no metric.
    try:
        configured = int(json.load(urllib.request.urlopen(f"{args.url}/config?key=retrieve.top_k"))["data"]["value"] or 3)
    except Exception:
        configured = 3

    effective = min(args.k, configured)
    if effective != args.k:
        print(f"note: server top_k={configured}, so this is precision@{effective}, not @{args.k}")
        args.k = effective

    if not args.sweep:
        hits, total, misses = evaluate(args.url, queries, args.k)
        print(f"precision@{args.k}: {hits}/{total} = {hits / total:.3f}")

        for q, want, got in misses:
            print(f"  MISS {q!r}\n       {want}\n       top was {got[:70]!r}")

        return 0 if hits == total else 1

    key, values = args.sweep.split("=", 1)
    original = json.load(urllib.request.urlopen(f"{args.url}/config?key={key}"))["data"]["value"]

    print(f"{key:>34} | P@{args.k}")

    try:
        for v in values.split(","):
            set_config(args.url, key, v)
            hits, total, _ = evaluate(args.url, queries, args.k)
            print(f"{v:>34} | {hits}/{total}")
    finally:
        # Never leave a swept knob changed — a benchmark that mutates live config is a footgun.
        set_config(args.url, key, original or "0")
        print(f"\nrestored {key} = {original or '0'!r}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
