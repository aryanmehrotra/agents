#!/usr/bin/env python3
"""
org-memory evaluation harness.

Compares the live dense-retrieval system (with its calibrated abstention floor) against a
BM25 lexical baseline, on a labeled query set:
  - ANSWERABLE queries  (query -> the one decision it should surface): retrieval quality.
  - UNANSWERABLE queries (off-topic): abstention correctness (returning NOTHING is the win).

No external dependencies (pure-stdlib BM25 + urllib). mem0/Zep are external services and are
listed as not-run baselines. Produces eval/RESULTS.md.

Honesty: every number here is measured on this corpus + this labeled set. The labeled set is
LLM-generated paraphrased questions (agents), so it is a proxy for real user queries, not gold.
"""
import json, math, os, re, sys, urllib.parse, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
API = os.environ.get("ORGMEM_API", "http://localhost:8000")
K = 3            # primary cutoff (top-k the system actually injects)
DEEP = 10        # depth for MRR

STOP = set("a an the of to in for on and or is are be with as at by from into this that it its "
           "how do i we you should when where what why can our my your not no".split())

def toks(s):
    return [t for t in re.split(r"[^a-z0-9]+", (s or "").lower()) if t and t not in STOP]

# ---------- data ----------
def load_corpus():
    with open(os.path.join(HERE, "corpus.json")) as f:
        return json.load(f)

def load_answerable():
    out = []
    for i in range(3):
        p = os.path.join(HERE, f"queries_{i}.jsonl")
        if not os.path.exists(p):
            continue
        with open(p) as f:
            for line in f:
                line = line.strip()
                if line:
                    out.append(json.loads(line))
    return out

def load_unanswerable():
    with open(os.path.join(HERE, "unanswerable.txt")) as f:
        return [l.strip() for l in f if l.strip()]

# ---------- dense system (live API) ----------
def api_config(key, value):
    body = json.dumps({"key": key, "value": str(value)}).encode()
    req = urllib.request.Request(API + "/config", data=body, headers={"Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=10).read()

def dense_recall(q, topk):
    url = f"{API}/recall?q={urllib.parse.quote(q)}"
    with urllib.request.urlopen(url, timeout=20) as r:
        d = json.load(r)["data"]
    ids = [it["decision"]["id"] for it in (d.get("items") or [])]
    return ids, bool(d.get("weak")), d.get("count", 0)

# ---------- BM25 baseline ----------
class BM25:
    def __init__(self, docs, k1=1.5, b=0.75):
        self.docs = docs
        self.k1, self.b = k1, b
        self.corpus = [toks(d["what"] + " " + (d.get("why") or "")) for d in docs]
        self.N = len(self.corpus)
        self.avgdl = sum(len(d) for d in self.corpus) / max(1, self.N)
        self.df = {}
        for d in self.corpus:
            for t in set(d):
                self.df[t] = self.df.get(t, 0) + 1
        self.idf = {t: math.log(1 + (self.N - n + 0.5) / (n + 0.5)) for t, n in self.df.items()}
        self.tf = [{} for _ in self.corpus]
        for i, d in enumerate(self.corpus):
            for t in d:
                self.tf[i][t] = self.tf[i].get(t, 0) + 1

    def rank(self, q, topk):
        qt = toks(q)
        scores = []
        for i, d in enumerate(self.corpus):
            dl = len(d); s = 0.0
            for t in qt:
                if t not in self.tf[i]:
                    continue
                f = self.tf[i][t]
                s += self.idf.get(t, 0) * (f * (self.k1 + 1)) / (f + self.k1 * (1 - self.b + self.b * dl / self.avgdl))
            if s > 0:
                scores.append((s, i))
        scores.sort(reverse=True)
        return [self.docs[i]["id"] for _, i in scores[:topk]]

# ---------- metrics ----------
def rank_of(ids, target):
    return ids.index(target) + 1 if target in ids else 0

def eval_system(name, answerable, unanswerable, recall_fn, abstains):
    r1 = r3 = mrr = 0
    abstained_answerable = 0
    for item in answerable:
        ids = recall_fn(item["query"], DEEP)
        pos = rank_of(ids, item["decision_id"])
        if pos == 1: r1 += 1
        if pos and pos <= 3: r3 += 1
        if pos: mrr += 1.0 / pos
        if abstains and len(ids) == 0: abstained_answerable += 1
    n = len(answerable)
    # unanswerable: correct behavior is to return nothing
    correct_abstain = 0
    for q in unanswerable:
        ids = recall_fn(q, K)
        if len(ids) == 0:
            correct_abstain += 1
    m = len(unanswerable)
    return {
        "system": name, "n_answerable": n,
        "recall@1": r1 / n, "recall@3": r3 / n, "mrr@10": mrr / n,
        "abstained_on_answerable": abstained_answerable / n if abstains else None,
        "n_unanswerable": m, "abstention_acc": correct_abstain / m,
    }

def main():
    corpus = load_corpus()
    answerable = load_answerable()
    unanswerable = load_unanswerable()
    print(f"corpus={len(corpus)}  answerable={len(answerable)}  unanswerable={len(unanswerable)}")

    # deepen the dense system so MRR@10 can see rank; restore after.
    prev_topk = 3
    api_config("retrieve.top_k", DEEP)
    try:
        dense = eval_system("dense (org-memory, w/ floor)", answerable, unanswerable,
                            lambda q, k: dense_recall(q, k)[0], abstains=True)
    finally:
        api_config("retrieve.top_k", prev_topk)

    bm = BM25(corpus)
    # BM25 has no abstention floor: it returns top-k for any non-empty term overlap.
    bm25 = eval_system("BM25 (lexical, no floor)", answerable, unanswerable,
                       lambda q, k: bm.rank(q, k), abstains=False)

    rows = [dense, bm25]
    # write results
    lines = ["# Evaluation results", "",
             f"- corpus: **{len(corpus)}** decisions",
             f"- answerable queries: **{len(answerable)}** (LLM-paraphrased, target = the source decision)",
             f"- unanswerable queries: **{len(unanswerable)}** (off-topic; correct answer = return nothing)",
             "", "| system | Recall@1 | Recall@3 | MRR@10 | abstained-on-answerable | abstention-acc (unanswerable) |",
             "|---|---|---|---|---|---|"]
    for r in rows:
        aoa = "—" if r["abstained_on_answerable"] is None else f"{r['abstained_on_answerable']:.0%}"
        lines.append(f"| {r['system']} | {r['recall@1']:.0%} | {r['recall@3']:.0%} | {r['mrr@10']:.3f} | {aoa} | {r['abstention_acc']:.0%} |")
    lines += ["",
        "**Reading it.** Recall@k / MRR measure whether the right decision is surfaced for an answerable query. "
        "*abstention-acc* is the fraction of OFF-TOPIC queries correctly answered with nothing — the differentiator: "
        "a lexical/dense system with no calibrated floor always returns its top-k, so it scores ~0% here while injecting "
        "irrelevant context. *abstained-on-answerable* is the cost side: answerable queries the floor wrongly rejected.",
        "",
        "**Not run (external services):** mem0 (Chhikara et al. 2025), Zep (Rasmussen et al. 2025) — require standing "
        "deployments; wired as future baselines. The north-star withholding-A/B ('saves') needs an agent-in-the-loop and "
        "is specified in PAPER.md §7.",
        "",
        "_Caveat: the answerable set is LLM-generated paraphrase, a proxy for real queries, not human-gold labels._"]
    out = os.path.join(HERE, "RESULTS.md")
    with open(out, "w") as f:
        f.write("\n".join(lines) + "\n")
    print("\n".join(lines))
    print(f"\nwrote {out}")

if __name__ == "__main__":
    main()
