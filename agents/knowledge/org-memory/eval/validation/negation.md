# Negation / Opposite-Intent Validation — Decision Memory

**Target:** `http://localhost:8000/recall` (decision-memory, GoFr corpus)
**Date:** 2026-07-30
**Method:** 16 opposite-intent query pairs (exact-antonym + negation forms). Query both sides, compare TOP decision (`.data.items[0].decision`), similarity, and `.data.weak`. **A pair FAILS when the system returns the same top decision for the positive and the opposite/negated query** — i.e. it ignored the intent direction.

## Suspected failure (confirmed)

Dense retrieval matches the *topic*, not the *intent direction*. "How do I DISCONNECT from Redis" returns the "CONNECT to Redis" decision. Antonyms/negations ("disable", "remove", "stop", "NOT", "avoid", "without") barely move the ranking and never flip it to a correctly-negated decision.

## Results

| # | Topic | POS query | NEG query | Same top? | POS sim / weak | NEG sim / weak | Verdict |
|---|-------|-----------|-----------|-----------|----------------|----------------|---------|
| 1 | Redis connect | connect to Redis | **disconnect** from Redis | YES | 0.856 / F | 0.795 / **F** | FAIL (confident) |
| 2 | Cache | add a cache | **remove** the cache | no | 0.753 / T | 0.716 / T | drift |
| 3 | Tracing | enable tracing | **disable** tracing | YES | 0.795 / T | 0.733 / T | FAIL |
| 4 | Auth | enable authentication | **disable** authentication | YES | 0.838 / T | 0.763 / T | FAIL |
| 5 | Circuit breaker | enable circuit breaker | **turn off** circuit breaker | YES | 0.830 / F | 0.785 / **F** | FAIL (confident) |
| 6 | Transactions | should I **use** a txn | should I **avoid** a txn | YES | 0.703 / T | 0.704 / T | FAIL (NEG sim higher) |
| 7 | Retry | when SHOULD we retry | when should we **NOT** retry | YES | 0.732 / T | 0.743 / T | FAIL (NEG sim higher) |
| 8 | Middleware | add middleware | **remove** middleware | no | 0.740 / T | 0.705 / T | drift |
| 9 | Server | **start** the server | **stop** the server | no | 0.798 / T | 0.733 / T | drift |
| 10 | DB connection | **open** a db connection | **close** a db connection | no | 0.796 / T | 0.753 / T | drift |
| 11 | Pub/Sub | **subscribe** to topic | **unsubscribe** from topic | no | 0.821 / T | 0.764 / T | drift |
| 12 | Metrics | enable metrics | **disable** metrics | no | 0.826 / F | 0.765 / T | drift |
| 13 | Migrations | **run** migrations on startup | **skip** migrations on startup | YES | 0.719 / T | 0.715 / T | FAIL |
| 14 | Cron | **register** a cron job | **unregister** a cron job | YES | 0.778 / T | 0.733 / T | FAIL |
| 15 | Logging | should I log every request | should I **NOT** log every request | YES | 0.722 / T | 0.691 / T | FAIL |
| 16 | TLS | enable TLS | run **without** TLS | no | 0.732 / T | 0.690 / T | drift (both off-topic) |

weak: T = `weak:true` (hedged), F = `weak:false` (confident).

## Findings

- **Confusion rate: 9/16 = 56%** returned the *identical* top decision for directly opposite intents.
- **Intent was NEVER correctly respected.** In 0/16 pairs did the negated query retrieve a decision that actually matches the opposite intent. The 7 "drift" (non-FAIL) cases only escaped because the negation nudged retrieval to a *different but still wrong* decision (topic drift) — not to a correctly-negated one. The corpus simply has no "disconnect/disable/avoid" decisions, so the best case is drift, never correctness.
- **Negation words are near-invisible to the embedder.** NEG similarity is typically ~0.03–0.06 below POS (same decision, slightly lower score). In pairs #6 (transactions) and #7 (retry — a `NOT` form), the **negated query scored *higher*** than the positive one against the same directional decision — the model gave the opposite-intent query *more* confidence in the wrong advice.
- **`weak` does not reliably hedge the wrong direction.** Of the 9 failures, 7 carried `weak:true` on the negated side (partial hedge), but **2 failures (#1 Redis disconnect, #5 circuit-breaker off) returned `weak:false`** — full confidence while handing back the exact opposite of what was asked. The hedge is a byproduct of absolute similarity thresholding, not of any negation awareness, so it cannot be relied on as a guardrail.

## Verdict

**FAIL.** The decision-memory does not model intent direction. Retrieval is topic-driven; antonyms and explicit negations ("NOT", "avoid", "without", "disable") do not flip — and sometimes *raise* — the ranking of the positive-direction decision. Over half of opposite-intent pairs collapse to the same top decision, and the `weak` flag does not consistently hedge the wrong-direction answer (2 of 9 failures were served confidently). This is the documented negation-blindness of bi-encoder dense retrieval.

## Recommended fix

Add a **cross-encoder re-ranker** over the top-k dense candidates (NevIR; Nogueira & Cho, "Passage Re-ranking with BERT"). A cross-encoder jointly encodes query+candidate and is far more sensitive to negation/direction than a bi-encoder, so it can down-rank a "connect"/"enable" decision for a "disconnect"/"disable" query. Pair it with a hard directional guard: if the best re-ranked candidate still carries opposite-polarity intent, force `weak:true` (or return no confident decision) rather than serving the wrong-direction advice.
