# Cross-encoder re-ranking — shipped, and it did NOT move the score. Here is why that matters.

**Date:** 2026-07-30 · **Model:** `gpustack/bge-reranker-v2-m3-Q8_0` (635 MB GGUF)
**Runtime:** Kronk (llama.cpp/GGUF) **in-process** — no daemon, no container, no ONNX, Metal on M4
**Result:** precision@3 **15/18 → 15/18.** Shipped disabled by default.

---

## 1. The model does what it was chosen for

Probed in isolation on this system's exact failures, with contrasting candidates present:

| test | outcome |
|---|---|
| `should we fail closed when redis is down` | ✅ "Fail **closed** on auth service errors" ranked #1 at **6.5×** the "fail open" score |
| `do not retry failed requests` | ✅ "**Do not** retry non-idempotent writes" ranked #1 at **22×** the "retry" score |
| `what is the source of truth for orders` | ❌ `order_events` still beat `orders` (0.214 vs 0.164) |

So: **negation, decisively fixed. Entity conflation, not fixed.** Those two failures are NOT the same
root cause, which contradicts what this project's own docs (RESEARCH.md §10d) had been asserting.

Latency, in-process: **50–70 ms warm** for a small candidate set, 0.4 s model load from cache.

## 2. Deployment: two dead ends before one that worked

- **HuggingFace TEI** — the obvious choice, has a first-class `/rerank`. **Publishes no arm64 image**;
  `docker run` fails outright on Apple Silicon, and amd64 emulation is far too slow to measure.
- **ONNX / fastembed** — works on arm64, but rejected: it drags a Python runtime into a project whose
  defining property is a single Go binary.
- **Kronk** (llama.cpp/GGUF, in-process via yzma) — already a dependency elsewhere in this repo,
  exposes `Rerank()` with `PoolingTypeRank`, needs no daemon and no HTTP hop, and gets Metal for free.

## 3. It still didn't move the golden set — and the reason is the finding

**The three failing queries are not ranking failures. They are CORPUS GAPS.**

```
"should we fail closed when redis is down"
   fail-closed decisions in corpus:  11
   ...that mention Redis:             0     ← 9 of the 11 are about recommender rules
"do not retry failed requests"
   decisions advising against retry:  0
```

There is no right answer to retrieve. Re-ranking **reorders; it cannot invent.** No reranker, at any
size, fixes a question the corpus cannot answer.

The correct output for both queries is **abstention** — and abstention is a decision made by the
*gate*, not by the ranker.

## 4. The subtler result: a cross-encoder is a poor abstention signal

The obvious next move is to gate on the cross-encoder score instead of cosine. Measured, that fails:
asked `should we fail closed when redis is down` against a pool with **no** fail-closed alternative,
the model scored the "**Fail open**" decision **0.97**.

The same model that separates the polarities 6.5× *when both are present* rates the wrong-polarity
document as near-perfect *when it is the only candidate*. That is inherent: a cross-encoder scores
each pair independently, and "fail open when Redis is unavailable" shares almost every token with
"should we fail closed when redis is down". It is a **relative** comparator, excellent at choosing
between alternatives, and it carries no calibrated notion of "none of these are right".

**Consequence:** the cross-encoder should decide ORDER; it must not be trusted to decide RELEVANCE.
The wide-recall + rerank-gate wiring was built and is retained, but the gate stays on the fitted
score-distribution model (`scoredist.go`), which does have a calibrated abstention semantics.

## 5. What changed in the codebase

- Wide-recall cascade: with a reranker attached, cosine becomes pure RECALL (top `rerank_recall_n`,
  ungated) and the cross-encoder re-scores. This was necessary and not sufficient — gating on cosine
  first meant only 3 decisions survived for the fail-closed query, none of them relevant, so
  re-ranking could only reshuffle three wrong answers. **A cascade cannot rescue what its first stage
  discarded** (Wang/Lin/Metzler, SIGIR 2011).
- Graceful degradation: any reranker failure — model missing, load error, budget exceeded — returns
  the dense ordering untouched. Verified live: `rerank_recall_n=400` blew the 5 s budget and the
  system silently served dense results rather than failing the recall.
- `Reranker` is an interface, so ranking stays testable without a 635 MB model.

## 6. Honest status

| | |
|---|---|
| Negation blindness | **unchanged at the system level** — the model can fix it, the corpus cannot exercise it |
| Entity conflation | **not fixed** — cross-encoder fails it too, as BM25 did |
| precision@3 | **15/18**, same as dense-only |
| Default | **OFF** (`ORGMEM_RERANK_MODEL` unset) — it costs 635 MB and buys nothing measurable *here* |

**What would actually move the number:** capture the missing decisions. A memory cannot answer what
it was never told, and three of this project's flagship "retrieval defects" turn out to be that.
The near-miss abstention question added in `ask.go` — *"I found nothing, but something came close.
Should I have known it?"* — is aimed at exactly this, and is now the higher-value lever.

**Kept anyway** because the negative result is precise: the model works, the wiring works, the gap is
in the data. A corpus with genuine polarity pairs would benefit immediately, and this file records
what to re-measure when that exists.
