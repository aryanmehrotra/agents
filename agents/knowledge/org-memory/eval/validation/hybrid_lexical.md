# Lexical/BM25 Hybrid — executed evaluation, and an honest NEGATIVE result

**Date:** 2026-07-30 · **Corpus:** 1643 surfaceable · **Live:** http://localhost:8000
**Verdict: does not help this corpus. Shipped DISABLED by default (`rank.w_lexical = 0`).**

---

## Why it was built

`RESEARCH.md §10c` listed a "lexical/BM25 hybrid gate to kill topically-near-but-off-intent hits" as
a grounded path, and both of the system's open retrieval failures looked like they should yield to
exact-token matching:

- **entity conflation** — `orders` vs `order_events` differ by one identifier
- **negation blindness** — "fail open" vs "fail closed" differ by one token

The reasoning was sound and the citations are real (Robertson & Zaragoza FnTIR 2009; Thakur et al.
BEIR NeurIPS 2021 on dense retrievers underperforming on entity-heavy queries; Cormack et al. SIGIR
2009 for rank fusion). The measurement disagrees with the reasoning.

## Method

BM25 over `what` + `why`, identifiers preserved as single tokens (`orders` is NOT split), fused
with the dense signal by Reciprocal Rank Fusion (scale-immune, no normalisation constant to tune).
Weight swept over `rank.w_lexical ∈ {0, 0.1, 0.25, 0.5, 1.0, 2.0}` against a 15-query set with a
required substring in the top-3, plus 3 polarity pairs.

## Result 1 — precision@3: the hybrid is neutral-to-harmful

| `rank.w_lexical` | P@3 |
|---|---|
| **0 (dense only)** | **15/15** |
| 0.1 | 14/15 |
| 0.25 | 14/15 |
| 0.5 | 14/15 |
| 1.0 | 14/15 |
| 2.0 | 14/15 |

Every non-zero weight loses the same query: `which database is the source of truth for order data`.
The cause is visible and instructive — that query does **not contain the token `orders`**. It
says "order data". BM25 therefore has no identifier to match on and scores what it *can* match:
"source", "truth". That promotes *"The README is the single source of truth for current pipeline
behavior"* — a decision about READMEs, topically unrelated, but lexically an excellent match for the
query's most common words.

**The hybrid only helps when the query already names the exact identifier. When it does, dense
retrieval was getting it right anyway** (`orders table storage` and
`order_events source of truth` both hit at every weight, including 0).

## Result 2 — negation: the hybrid makes it WORSE

Identical-top across a polarity pair (lower is better):

| `rank.w_lexical` | identical top |
|---|---|
| **0** | **1/3** |
| 0.5 | 2/3 |
| 1.0 | 2/3 |

This is the opposite of the hypothesis, and in hindsight it is obvious. "add retries to failed
requests" and "do not retry failed requests" share `retry`, `failed`, `requests` — three
high-signal content tokens — and differ by `do`/`not`, which are short, common, low-idf, and in most
stoplists. **BM25 weights the shared content terms far above the negation, so it pulls the two
polarities together harder than the embedding does.** Adding a lexical channel strengthens exactly
the wrong evidence.

## Conclusion

- The BM25 hybrid path documented in RESEARCH.md §10c is **measured and rejected for this corpus.**
  It is not a partial win to be tuned; at every weight tried it is neutral or negative on both
  problems it was meant to solve.
- The code ships **disabled by default**, so this costs nothing and remains available for a corpus
  with different characteristics (heavy rare-identifier queries, or a domain where the embedding
  model is weaker). The index is built lazily and never built at all while the weight is 0.
- **This strengthens the case for the cross-encoder**, and narrows it. NevIR (Weller et al., EACL
  2024) measured that cross-encoders handle negation and bi-encoders do not — a joint query+document
  read, not a second bag-of-words channel. BM25 was the cheap approximation of "look at the actual
  tokens"; the measurement says the cheap approximation does not capture what makes the cross-encoder
  work.

**Retained anyway:** the negative result itself is worth more than the code. A documented "grounded
path" has now been executed and closed, so it will not be proposed again as an untried idea — and
the reason it fails (low-idf negations, shared content terms) is a property of the problem, not of
this implementation.
