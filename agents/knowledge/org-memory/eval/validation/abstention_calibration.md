# Abstention Calibration Validation

**Target:** `http://localhost:8000` decision-memory (`GET /recall`)
**Corpus:** 1643 real engineering decisions (acme services, GoFr how-to, CS/architecture books)
**Date:** 2026-07-30
**Method:** Two hand-labeled probe sets of 32 questions each, one query per question, `count==0` treated as abstention.

- **ANSWERABLE (32):** natural questions derived by browsing `GET /decisions` — gateway/tenancy, recommender events & metrics, aggregator cost/billing, GoFr how-to (CRUD, shutdown, migrations, metrics port, RBAC, BadgerDB), plus a few book-backed ones (Go slice aliasing, URL-shortener design, clean-code naming).
- **UNANSWERABLE (32):** off-topic questions the corpus cannot answer — geography, cooking, sports, gardening, astronomy, consumer trivia, and 2 gibberish strings. Deliberately avoids software topics the books cover.

## Headline rates

| Metric | Result |
|---|---|
| **False-abstain** (answerable → `count==0`) | **0 / 32 = 0.0%** |
| **False-inject** (unanswerable → `count>0`) | **0 / 32 = 0.0%** |

Every answerable question returned decisions; every off-topic/gibberish question abstained. Clean separation at the `count` level.

## Confusion table

|                       | Returned results (`count>0`) | Abstained (`count==0`) |
|-----------------------|:---:|:---:|
| **Answerable (32)**   | 32 (true-answer)  | 0 (false-abstain) |
| **Unanswerable (32)** | 0 (false-inject)  | 32 (true-abstain) |

## Similarity separation

- Answerable hits `top_similarity`: min **0.694**, median **0.793**, max **0.885**.

> **CORRECTED 2026-07-30** (META_AUDIT correction #2). The claim below is withdrawn, and the fix went
> further than a rewording: the number it rested on could not be measured at the time.
>
> ~~Every off-topic query returned `top_similarity = 0.000` and `count = 0` — no near-misses, no
> borderline injections. The margin between the weakest real hit (0.694) and the strongest off-topic
> (0.000) is enormous; the abstention gate has very comfortable headroom on this set.~~
>
> `top_similarity` was computed as the maximum over the *surfaced* items. On an abstention that list
> is empty, so the value was always exactly `0.000` — an **absence, not a measured cosine**. No
> margin could be derived from it, and "the strongest off-topic scored 0.000" was never a finding
> about the corpus. A query for `kubernetes ingress nginx annotations` reporting 0.000 against a
> corpus that demonstrably contains Kubernetes and Helm decisions is the tell.

`recall` now also returns `top_candidate_similarity` — the best cosine any surfaceable decision
achieved *before* the gate — alongside the `floor` it failed to clear. Re-measured on the fixed
build, same probes:

| Off-topic query | true best cosine | floor | short by |
|---|---|---|---|
| `how tall is mount everest` | 0.508 | 0.653 | 0.145 |
| `what is the capital of France` | 0.523 | 0.653 | 0.130 |
| `best chocolate chip cookie recipe` | 0.565 | 0.653 | 0.088 |
| `zxqv flurble womp qwerty` (gibberish) | **0.612** | 0.653 | **0.041** |
| `kubernetes ingress nginx annotations` | **0.621** | 0.653 | **0.032** |

**The corrected finding differs materially from the withdrawn one.** The real margin between the
weakest genuine hit (0.694) and the strongest off-topic near-miss (0.621) is **0.073** — not the
"enormous" 0.694-vs-0.000 gap — and the nearest miss sits **0.032** below the floor, about a third
of `weak_margin`. The gate holds on this set, but by a modest margin, not with "very comfortable
headroom".

Two things stand out that were invisible before, and both bear on how much the absolute cosine can
be trusted. Random gibberish scores **0.612**, within 0.041 of the bar. And the ordering is
uninformative — gibberish outscores a real English question about mountain heights. Whatever these
cosines measure, it is not distance-from-corpus in the way the original margin claim assumed. The
abstention *decisions* are correct on all 64 queries; the *confidence* in them should be described
as adequate, not comfortable.

## `weak` flag reliability

`weak` cannot be measured on false-injects (there were none — all off-topic abstained with `weak=false`). Assessed instead on the 32 genuine hits:

- **16 / 32 answerable hits were flagged `weak=true`.**
- `weak` is **not** a clean threshold on `top_similarity`. Everything below ~0.775 is flagged weak (reasonable hedging on borderline matches), but the 0.78–0.86 band is **mixed**: e.g. GoFr graceful-shutdown (0.851) and GoFr metrics-port (0.842) are flagged weak, while several 0.776–0.80 hits are not. So `weak` appears to reflect result-set cohesion (agreement among the lower-ranked items), not the top score alone.

**Does `weak` concentrate on the off-topic/irrelevant?** At the level that matters — off-topic vs on-topic — **yes, via abstention**: all irrelevant queries are dropped before `weak` is even in play. As a *fine-grained* confidence signal *within* returned results, `weak` is **noisy / over-cautious**: it fires on ~50% of genuine hits including some strong (0.84–0.85) matches, so a consumer treating `weak=true` as "probably wrong" would discard many correct answers. Its errors are conservative (hedging real answers) rather than dangerous (it never confidently endorsed a wrong answer, because there were no wrong answers to endorse).

## Verdict

**Abstention is well-calibrated on this evaluation.** The system's core honesty claim holds on the
decision itself: 0% false-inject and 0% false-abstain across 64 labeled queries. ~~with a wide
similarity margin between real hits and off-topic noise~~ — **corrected**: the measured margin is
0.073 (weakest hit 0.694 vs strongest off-topic near-miss 0.621), and the closest negative misses
the floor by 0.032. Adequate, not wide. The primary mechanism (returning `count==0` on off-topic
input) is doing the real work and doing it correctly.

**Scope limit on the 0% false-inject figure** (META_AUDIT correction #3 — promoted from the footnote
below, where it was too easy to miss). *0/32 holds for FAR-off-topic negatives only.* The audit
demonstrated the boundary case failing: `react useEffect cleanup best practices` — a software topic
genuinely absent from the corpus — **injected 2 results** at similarity 0.658, just above the 0.653
floor. The headline should be read as "0% false-inject on distant negatives; the near-domain
boundary does inject." An honest false-inject rate needs a negative set built from *uncovered
software topics*, not from cookie recipes and mountain heights.

**On the `weak` flag** — the finding below (16/32 genuine hits flagged weak, no monotonicity in
similarity) was confirmed independently by the audit and traced to a defect: `weak` OR-ed together
two unrelated signals, "the top match is barely above the bar" and "the top match doesn't separate
from its runner-up". The second fires constantly, because a good query legitimately matches several
near-equally-relevant decisions — that is redundancy in the corpus, not low confidence in the
answer. `recall` now reports the two separately as `weak` and `ambiguous`. Re-measured on the fixed
build, the queries that previously came back `weak=true` (circuit breaker, rate limiting, error
handling conventions) are now correctly `weak=false, ambiguous=true`. See
[`ranking_robustness.md`](ranking_robustness.md) for the re-run.
