# Phase-0 Correctness Audit — is every behaviour backed?

**Rule being enforced:** *Phase 1 does not start until Phase 0 is 100% correct.*
**Date:** 2026-07-30 · **Corpus:** 1643 surfaceable, `sha256[:16] = a8dca1a6d7b7a83a`

Two claims define Phase 0, and this audit tests both against the code rather than against the plan:

1. **Gate #0** — *"No behavior is a constant in code. Every tunable is a config value."*
2. **RESEARCH.md, line 1** — *"org-memory is not a pile of heuristics. Every non-obvious design
   decision traces to an established result."*

**Both are currently false.** Details below, each with the fix or the citation that repairs it.

---

## 1. Gate #0 violations — behaviour that is NOT configurable

Gate #0 is described in the plan as "non-negotiable; gates EVERY component and EVERY decision," with
the acceptance test: *"if anyone asks 'can we tune X without shipping code?' the answer is always
yes."* Four places answer no.

| # | Violation | Where | Impact |
|---|---|---|---|
| G1 | **Recency half-life hardcoded at 30 days** | `rank.go:302` — `math.Exp(-days / 30.0)` | Not tunable; and see §2 — it silently disagrees with the configurable half-life |
| G2 | **Scope-specificity divisor hardcoded at 3** | `rank.go:313` — `float64(spec) / 3.0` | The importance curve cannot be reshaped without a deploy |
| G3 | **No build-time literal scanner** | absent | §1.7 requires *"no literal constants (scanner fails the build)"*. Nothing enforces Gate #0, which is why G1/G2 survived |
| G4 | **No config versioning or audit trail** | `config.go` — plain `map[string]string` | §0 requires every change *"validated, versioned, and audited (who/what/when — same append-only discipline as the memory itself)"*. Validation exists; versioning and audit do not |

**G3 is the root cause.** The gate was stated as non-negotiable and then enforced by intention alone.

---

## 2. 🔴 Correctness bug — two different decay curves over the same quantity

Both functions decay on `now − d.Updated`, in the same scoring pass, on the same decision:

```go
// rank.go:302  — ranking recency prior
return math.Exp(-days / 30.0)                    // half-life ≈ 20.8 days (30 = the time CONSTANT)

// forget.go:34 — retrievability / forgetting
fresh = math.Exp(-ageDays / math.Max(1, half))   // half = forget.halflife_days = 90
```

Three separate defects:

1. **They disagree.** One says a decision's freshness is spent in weeks, the other in months. Nothing
   reconciles them, and no document mentions there are two.
2. **Both mislabel the constant.** `exp(−t/τ)` has *time constant* τ; the half-life is `τ·ln2`. So
   `halflife_days = 90` actually produces a **62.4-day** half-life, and the "≈30-day half-life-ish"
   comment on `recencyScore` describes a **20.8-day** half-life. Both knobs are off by a factor of
   `ln2 ≈ 0.693` from their stated meaning.
3. **Neither value is fit to anything.** 30 and 90 are unexplained.

The correct form is one decay function, one configurable constant, correctly named — and the value
fit from data, not chosen (see R6).

---

## 3. Knob inventory — 28 knobs, backed vs asserted

✅ backed · 🟡 partially backed (form cited, value arbitrary) · ❌ asserted with no basis

| Knob | Default | | Basis |
|---|---|---|---|
| `retrieve.precision_floor` | 0.30 (live: 0.6528) | ✅ | Chow 1970 reject option; **+ R1, R2 below** |
| `retrieve.calibration_margin` | 0.03 | 🟡→✅ | auto-calibration = noise-ceiling estimation; **R1 names it** |
| `retrieve.top_k` | 3 | ✅ | Liu et al. 2023 *Lost in the Middle*; **+ R2** for principled truncation |
| `retrieve.max_top_k` | 50 | ✅ | DoS clamp — engineering, no citation needed |
| `retrieve.weak_margin` | 0.10 | ❌ | pure magic number; **R3** gives the principled form |
| `retrieve.weak_min_margin` | 0.03 | ❌ | pure magic number; **R3** |
| `retrieve.adaptive_z` | 0 (off) | ❌ | "2.0 is a near-universal knob" is asserted; z=2 is a convention, not a result. **R3** |
| `retrieve.adaptive_min_n` | 8 | ❌ | **too small.** σ from n=8 is very noisy; the adaptive floor is `mean + z·σ`, so it inherits that noise |
| `retrieve.hard_scope` | 0 (soft) | ✅ | Tulving & Thomson 1973 encoding specificity |
| `rank.w_relevance` | 1.0 | ✅ | cascade ranking, Wang/Lin/Metzler 2011 |
| `rank.prior_cap` | 0.01 | 🟡 | *form* is now correct (Lee 1997 normalization). **Value is arbitrary** — and measured inert: max authority effect 0.0011 vs median adjacent gap 0.011 |
| `rank.w_recency` | 0.3 | ❌ | arbitrary |
| `rank.w_importance` | 0.3 | ❌ | arbitrary |
| `rank.w_retention` | 0.3 | ❌ | arbitrary |
| `rank.w_authority` | 0.3 (live 0.15) | 🟡 | EigenTrust backs *seeded* authority; the weight is arbitrary |
| `rank.w_feedback` | 0.3 | ❌ | arbitrary (added 2026-07-30) |
| `rank.mode` | capped | ✅ | capped vs RRF compared empirically |
| `rank.rrf_k` | 60 | ❌→✅ | **R4** — 60 is Cormack et al.'s measured TREC optimum |
| `rank.rrf_w_relevance` / `_prior` | 1.0 / 0.2 | ❌ | arbitrary |
| `forget.halflife_days` | 90 | 🟡 | Bjork & Bjork 1992 backs the *two-strength model*, not the number. **R6** |
| `forget.floor` | 0.03 | ❌ | arbitrary |
| `forget.reinforce_per_use` | 0.5 | ❌ | arbitrary |
| `forget.penalty_per_negative` | 0.5 | ❌ | arbitrary |
| `feedback.boost_per_helpful` | 0.1 | ❌ | arbitrary |
| `feedback.demote_per_notrel` | 0.1 | ❌ | arbitrary |
| `feedback.wrong_quarantine_at` | 2 | ❌ | arbitrary — but a *policy* choice, so "org-set" is a legitimate answer |
| `authority.default_role` / `authority.*` | sde2 / 1.0-0.3 | ✅ | policy by design, externally seeded (EigenTrust) |

**Score: 9 backed · 5 partial · 14 asserted.** The claim "every non-obvious design decision traces to
an established result" holds for the *architecture* and fails for the *numbers*.

---

## 4. Research found for the gaps

### R1 — the auto-calibrated floor already IS a published method
**Arampatzis & Kamps, "A signal-to-noise approach to score normalization," CIKM 2009.**
The system boots, probes with generic off-topic queries, measures the resulting similarity ceiling
(the *noise*), and sets the floor just above it. That is a signal-to-noise normalization, and it was
invented, named and evaluated in 2009. This is the single strongest existing design decision in
Phase 0 and it is currently uncited — described in `configs/.env` as if it were a local trick.
`calibration_margin` becomes the SNR offset, which the literature gives a basis for choosing.

### R2 — where to stop a ranked list is a solved problem
**Arampatzis, Kamps & Robertson, "Where to stop reading a ranked list? Threshold optimization using
truncated score distributions," SIGIR 2009.** Directly the `precision_floor` + `top_k` question:
choose the cutoff by fitting the score distribution and optimizing the target effectiveness measure,
rather than fixing a cosine number or a constant k. Modern learned variant: **Bahri et al., "Learning
to Truncate Ranked Lists for Information Retrieval," arXiv:2102.12793.**

### R3 — `weak` / `ambiguous` thresholds have a distributional form
The standard model is a **normal–exponential mixture**: relevant-document scores ≈ normal,
non-relevant ≈ exponential (**Manmatha, Rath & Feng, SIGIR 2001** — already cited for the *idea*;
**Arampatzis & Robertson, "Modeling score distributions in information retrieval," Information
Retrieval 2011** for the modern treatment). Fitting that mixture yields `P(relevant | score)`
directly, which replaces `weak_margin = 0.10` and `weak_min_margin = 0.03` with a calibrated
probability — and replaces `adaptive_z` with a fitted quantity rather than a chosen sigma count.

### R4 — `rrf_k = 60` is legitimate, just uncited
**Cormack, Clarke & Buettcher, SIGIR 2009** determined k=60 as the best average on TREC; subsequent
benchmarks find k ∈ [40, 80] comparable. The default is correct — it only looked arbitrary.

### R5 — `adaptive_min_n = 8` is statistically too small
No paper is needed to see the problem: the adaptive floor is `mean + z·σ`, and σ estimated from 8
samples has a relative standard error near 25%. With the corpus at 1643, this path effectively never
falls back — but the knob is a trap for a small deployment, exactly where auto-calibration matters
most. Raise it, or drop the adaptive path in favour of R1/R3, which estimate the same thing properly.

### R6 — a single global half-life is the naive form
**Settles & Meeder, "A Trainable Spaced Repetition Model for Language Learning," ACL 2016**
(half-life regression, Duolingo) — half-life is *regressed per item* from observed recall, not fixed
globally. The retention form `R = e^(−t/S)` is standard; what is unjustified is one constant `S` for
every decision in every org. This is the honest upgrade path for `forget.halflife_days`, and it is
also the fix for §2: one decay function, one correctly-named constant, fit rather than chosen.

---

## 5. Phase-0 gates from §1.7 — what is actually met

| Stage | Gate required | Status |
|---|---|---|
| Config | no-literal scanner fails build · live effect · bad config rejected · **audited** | ❌ scanner · ✅ live · ✅ rejected · ❌ audited |
| Capture | span-grounding: **hallucinated field must flag** · schema valid | ❌ **not built** — capture is trust-the-caller. This *is* the HIGH safety FAIL |
| Store | CRUD · supersede-never-delete · **tenant isolation (query can't cross)** | ✅ · ✅ · ⚠️ `hard_scope=0` by design ⇒ scope **cannot contain** an attacker |
| Retrieve | precision@k · inject-nothing below floor · placement | 🟡 golden set is small · ✅ validated 32/32 · ✅ |
| Performance | **p95 < budget** · budget-blown → dense-only fallback · soak | 🟡 **now measured** (see §5.1) · ❌ no fallback path · ❌ no soak |
| Safety | single-incident → quarantine · revocation propagates | 🟡 `wrong_quarantine_at=2`, untested end-to-end |

---

## 5.1 Performance — measured for the first time

**End-to-end** (live service, 1643 decisions, 120 requests, real Ollama): p50 **70.1 ms**, p95
**76.3 ms**, p99 79.9 ms. Of that, the Ollama embed round-trip alone is p50 22.2 / p95 29.8 ms.

**The scan** is the part that grows — it is exact brute-force cosine, O(N·dim). Benchmarked at
production width (768-d, `nomic-embed-text`; the in-repo fake embedder is 4096-d and overstates cost
by 5.3×, so an earlier run of this benchmark was pessimistic in the direction that would have
triggered premature ANN work):

| corpus | scan+rank |
|---|---|
| 1,000 | 1.12 ms |
| 10,000 | 12.25 ms |
| 25,000 | 87.29 ms |

At the current 1,643 decisions the scan is **≈1.8 ms** — comfortably inside any sane budget, and the
p95 is dominated by embedding, not by us.

> **⚠️ §1.6 of the plan overstates scale.** It claims the exact scan "remains sub-∼10 ms to roughly
> 10⁴–10⁵ decisions, beyond which an ANN index replaces it." Measured, the 10 ms line is crossed at
> **≈8,300 decisions**, and by 25,000 the scan is 87 ms — an order of magnitude off the claimed
> ceiling. Growth is also *superlinear* past ~10⁴ (2.5× the corpus costs 7.1× the time), consistent
> with the vector set leaving cache: 25,000 × 768 × 4 B ≈ 77 MB. The ANN swap is needed roughly **10×
> sooner than planned**. Nothing is broken today — but the number that decides when to build ANN was
> wrong, and now it is measured.

**Still missing:** the fallback the gate actually requires. §1.7 specifies "budget-blown → falls back
to dense-only (never blocks)"; there is no budget check and no fallback path, so an over-budget recall
simply takes as long as it takes.

## 6. The pending list, ordered

**Blocking correctness**
1. Unauthenticated `/capture` verbatim injection — HIGH, and it is the missing span-grounding gate
2. `Capture` silently drops `supersedes` on the dedupe path (`engine.go:63` returns before `:77`)
3. Two conflicting decay curves (§2), both mislabelled as half-lives
4. G1/G2 hardcoded constants → make them knobs; G3 scanner → enforce the gate

**Blocking the honesty claim**
5. 14 asserted numbers → adopt R1/R2/R3 so the floor and the confidence signals are *fitted*, not chosen
6. Cite R1 and R4 — two decisions that are already correct and merely undocumented
7. `adaptive_min_n = 8` (R5)

**Blocking "nothing ships unproven"**
8. Performance budget never measured; no fallback path exists
9. Config audit trail (G4)
10. Cross-encoder re-rank — negation + entity conflation (the one build that closes two known failures)

---

## 6.1 Fixed in this pass (2026-07-30)

| # | Item | What landed | Guard |
|---|---|---|---|
| 1 | **HIGH — unauth capture + verbatim injection** | admission gate on every mutating method (token, else loopback-only); captured text sanitised to one line before entering agent context; `[src:]` attestation now emitted only for org-verified provenance, else `[unverified src:]` | `trust_test.go` — 8 injection vectors, forged-marker test, admission matrix. Verified live: `POST /capture` → 401 without token, 201 with, `GET /recall` → 200 |
| 2 | `Capture` silently dropped `supersedes` on the dedupe path | shared `applySupersede` on both paths; self-supersede refused | `TestCaptureSupersedeOnReinforcePath`, `TestCaptureSelfSupersedeRefused` |
| 3 | Two conflicting decay curves, both mislabelled | one `halfLifeDecay(days, H)` using `ln2`, so a knob named half-life IS a half-life; recency half-life is now a knob | `TestHalfLifeMeansHalfLife`, `TestRecencyAndRetentionAgreeOnAge` |
| 4 | Gate #0 unenforced; 2 literals in the ranker | AST scanner over the behavioural files, allowlist-with-reason; 4 new knobs (`rank.recency_halflife_days`, `rank.importance_saturation`, `retrieve.max_precision_floor`, `hierarchy.max_inherit_depth`) | `TestGate0NoHardcodedBehaviour` — **fails the build** on a new literal |
| 9 | No config audit trail | append-only change log (key, from→to, by, scope, at) + `GET /config/history`; `SetBy` attributes the actor | `TestConfigChangesAreAudited` |
| — | Performance never measured | end-to-end p95 + scan-scaling benchmark at production embedding width | `bench_test.go` |

The decay fix changed behaviour: with the units corrected, memories persist ~44% longer for the same
knob value. Two existing tests asserted the *buggy* semantics (a 400-day decision falling below the
forget floor, which only happened because decay ran `ln2` too fast) and were corrected to 500 days —
the point at which a true 90-day half-life genuinely crosses the floor.

## 6.2 Thresholds are now fitted, not chosen (2026-07-30)

`scoredist.go` fits the **non-relevant** score distribution for each query and expresses every
confidence threshold against it. The knobs change from opaque cosine margins to quantities with units:

| was | is | meaning |
|---|---|---|
| `adaptive_z = 2.0` | `retrieve.max_false_inject_rate` | "at most this fraction of irrelevant decisions may clear the gate" |
| `weak_margin = 0.10` | `retrieve.weak_noise_p` | "flag weak above this chance the top match is noise" |
| `adaptive_min_n = 8` | `= 30` | σ̂ has 27% relative standard error at n=8, 13% at n=30 |

**A statistical defect was found and fixed in the process.** The old adaptive floor was `mean + z·σ`
over *all* candidate similarities — including the relevant ones it was meant to admit. Relevant
documents live in the right tail, so they inflated both moments: **the floor rose because the query
had good answers.** Measured (1600 noise + k relevant, threshold at α=0.01):

| relevant docs | 0 | 3 | 20 | 60 | 150 |
|---|---|---|---|---|---|
| old `mean+2σ` | 0.5715 | 0.5756 | 0.5979 | 0.6405 | **0.7075** |
| new one-sided | 0.5767 | 0.5770 | 0.5786 | 0.5824 | **0.5942** |

The old estimator drifts **+0.136**; the new one **+0.018** — ~8× more stable. The estimator is
one-sided (σ̂ from `median − Q1`, never touching a score above the median) because the contamination
is one-sided. Plain MAD was tried first and still drifted 11.6%, since it averages deviations on both
sides. This is the "truncated score distribution" of Arampatzis, Kamps & Robertson (SIGIR 2009) made
concrete; robust-scale foundations Rousseeuw & Croux (JASA 1993).

**Live, on the real corpus:**

| query | top | `noise_p` | weak |
|---|---|---|---|
| graceful shutdown | 0.762 | 2e-15 | false |
| should we fail open when redis is down | 0.804 | 7e-13 | false |
| error handling conventions | 0.755 | 1e-06 | false |
| `kubernetes ingress nginx annotations` (abstained) | 0.621 | 7e-04 | — |
| `zxqv flurble womp qwerty` (gibberish, abstained) | 0.612 | 1.3e-03 | — |
| **`react useEffect cleanup`** (the known false-inject) | 0.658 | **1.1e-03** | **true** ✅ |

The near-domain false-inject that the original validation missed — 2 results injected at 0.658 — is
now **correctly flagged**. Note also that gibberish (1.3e-3) and the react query (1.1e-3) land at
essentially the same confidence, which is right: neither is distinguishable from noise. Raw cosine
reported 0.612 vs 0.658 and made them look meaningfully different.

> **Honest limitation.** `noise_p` is a *relative* measure — it compares a score to that query's own
> noise distribution. A truly off-topic query has a tight, low distribution, so a modest top score can
> look "extreme" relative to it: `what is the capital of France` scores `noise_p = 3.6e-5`, more
> confident-looking than the react query, despite being far more off-topic. This is why the gate takes
> `max(absolute floor, fitted floor)` and the absolute floor remains a required backstop. The fitted
> threshold replaces the *arbitrary* part of the decision, not the whole decision.

**Default is off.** `max_false_inject_rate` defaults to 0 (disabled), so existing deployments are
byte-identical until they opt in. The `weak` signal is live.

## 6.3 Re-scoping — what is Phase 0, and what was never Phase 0

The pending list had drifted: several items on it were *correctness-layer* work wearing a Phase-0
label. Phase 0's job is narrower and should be judged on its own terms.

**Phase 0 = raw retrieval quality.** Given a context, surface the few genuinely relevant prior
decisions, or nothing. It never claims a decision is *right* — only that it is *relevant*, and it
abstains when it cannot tell. Everything Phase 0 owns must be excellent **without any correctness
machinery underneath it.**

| Moved OUT → Phase 1 | Why it is not Phase 0 |
|---|---|
| Weak-supervision extraction at capture | Requires the label model (Ratner & Ré) — the Phase-1 core |
| Hallucination / span-grounding **detection** | Needs multiple blind checkers + consensus; a single deterministic check cannot tell "ungrounded" from "grounded in a source we can't see" |
| Dawid–Skene consensus, Platt/temperature calibration | Definitionally the correctness layer (§3) |
| Conformal abstention gate | Upgrades Phase-0's floor *later*; the floor must first be a fitted quantity, which §6.2 now makes it |
| Learning-to-defer | Needs a calibrated risk estimate to defer *on* |
| Exogeneity firewall, blind parallel voting | Invariants that only bind once checkers vote |
| Human-confirm threshold at capture | A policy on a confidence score Phase 0 does not compute |

**What Phase 0 keeps** — admission stays a Phase-0 concern, because an open write endpoint is a
security property, not a correctness one. That half is already closed (§6.1): authenticated writes,
sanitised rendering, unforgeable trust markers. What Phase 0 does *not* owe is judging whether an
authenticated, well-formed decision is *true*.

### The re-scoped Phase-0 list

| # | Item | Status |
|---|---|---|
| P0-1 | Lexical/BM25 hybrid retrieval | ❌ **built, measured, REJECTED** — `eval/validation/hybrid_lexical.md` |
| P0-2 | Cross-encoder re-rank | ⏸ **blocked, with evidence** — see §6.4 |
| P0-3 | Hard-facet prefixes → config | ✅ **done** |
| P0-4 | precision@k golden set | ✅ **done** — 18 queries, **15/18**, committed + reproducible |
| P0-5 | Perf budget + non-blocking degradation | ✅ **done** |
| P0-6 | Tenant isolation that scope cannot bypass | ✅ **done** |

### The golden set now shows the negation defect — and a measurement bug it exposed

`precision@3` went from a clean-looking **15/15** to an honest **15/18** by adding three negation
queries. All three fail, all in the most dangerous way available:

| query | what came back |
|---|---|
| should we fail **closed** when redis is down | *"**Fail open** (allow the request)…"* |
| **do not** retry failed requests | *"**Retry** failed calls with exponential backoff…"* |
| **disable** rate limiting | *"**Implement** rate limiting server-side…"* |

**The earlier measurement of this was wrong, and too generous.** The negation check in
`negation.md` and in the BM25 sweep scored a pair as broken only when both polarities returned the
*same decision id* — which counted **1 of 3**. Two pairs returned *different* decisions carrying
*reversed advice*, and that is the worse failure, not the milder one: a different id looks like the
retrieval worked. Checking for the opposite **content** rather than an identical id shows **3 of 3**.

The gate therefore carries two criteria: `must_contain` (we missed the answer) and
`must_not_contain` (we returned the opposite). A `must_contain`-only harness is structurally blind
to inverted advice — it scores it as an ordinary miss, indistinguishable from harmlessly finding
nothing. For a negation query, **abstaining is a pass**: "I have no rule about failing closed" is
correct and honest; "here is the rule about failing open" is not.

This is now the number any negation fix has to beat. Until then the failure is visible on every run
instead of hidden behind a perfect score.

## 6.4 P0-2 cross-encoder — blocked, and here is the measurement

Not "didn't get to it". Tested, and the local infrastructure cannot support it:

- **Ollama cannot host a cross-encoder at all.** It serves embeddings and generation; reranking needs
  a sequence-classification head. There is no `/api/rerank`, and `bge-reranker-v2-m3` /
  `mxbai-rerank-large-v1` both 404 in its library.
- **The published fallback was tested and failed.** LLM-as-reranker is a real technique (RankGPT —
  Sun et al., EMNLP 2023), and `llama3.2:3b` is available, so it was evaluated on the two failures a
  reranker exists to fix:

| test | result |
|---|---|
| **negation** — "should we fail CLOSED" vs a "fail open" decision | ✅ correctly rejected the opposite-polarity candidate |
| **entity** — source of truth for `orders` vs `order_events` | ❌ **0/5 correct**, even with a prompt explicitly stating "a candidate about a DIFFERENT table does not answer the question" |

The model reproduces the *exact* failure the bi-encoder makes, so it cannot fix it. And the cost of
being wrong is high: **p50 156 ms, max 477 ms per rerank call, against a whole-service p95 of 76 ms**
— one rerank would more than double end-to-end latency on a component that sits in an agent's prompt
path.

**Conclusion:** P0-2 needs a real cross-encoder (a `bge-reranker`-class model served by something
that can host one — sentence-transformers, TEI, or an API), not a local generative model. It stays
open, and it remains the only live candidate for negation blindness — but it is an infrastructure
decision, not a coding task, and it should not be attempted on this stack.

**P0-1 is the highest-value item in the project right now.** Both open retrieval failures —
negation blindness and entity conflation — have the same cause: a bi-encoder scores topical
similarity and cannot see that `orders` ≠ `order_events`, or that "do not retry" ≠
"retry". Exact lexical matching sees precisely those distinctions, and the hybrid is already the
documented path in RESEARCH.md §10c. It needs no model, unlike P0-2.

## 7. Verdict

Phase 0's **architecture** is well-grounded — the gates, the abstention principle, the store
invariants, the cascade ranking, the soft-context prior all trace to real results, and two of its
best ideas (auto-calibrated floor, RRF k) turn out to be published methods the repo simply never
cited.

Phase 0's **parameters** are not grounded, and one of them is contradictory. The system is currently
honest about *when it does not know* and silent about *why its thresholds are what they are*. For a
project whose entire moat is calibrated honesty, the second gap is the one that matters: a
`precision_floor` nobody can justify is the same category of problem as the `top_similarity: 0.0`
that this validation round already caught — a number that looks like a measurement and is not.

**Not ready for Phase 1** — but materially closer than when this audit opened. The HIGH is closed,
Gate #0 is enforced by a test rather than by intention, the decay contradiction is gone, and
performance has a number for the first time.

**What still blocks Phase 1, honestly:**

1. **14 asserted parameters** (§3). The research to fix them is now in `RESEARCH.md §11` — fitting
   the score distribution replaces `weak_margin`, `weak_min_margin`, `adaptive_z` and grounds
   `precision_floor` — but the fitting itself is unbuilt. This is the one that matters: the moat is
   calibrated honesty, and a threshold nobody can justify is not calibration.
2. **Capture is still trust-the-caller.** Admission is now authenticated, which stops the *anonymous*
   attack, but there is no span-grounding gate — an authenticated caller (or a compromised one) can
   still write an ungrounded decision. §1.7's actual gate is "inject a hallucinated field → must
   flag", and that is Phase-1 weak-supervision work.
3. **No performance fallback**, and the ANN crossover arrives ~10× sooner than §1.6 assumed.
4. **Cross-encoder re-rank** — negation blindness and entity conflation both remain open, and one
   build closes both.

The honest read: Phase 0's *architecture* is sound and now provably so in more places than before.
Its *numbers* are still mostly chosen rather than measured, and that is exactly the gap Phase 1's
calibration machinery is supposed to close — which is an argument for fitting the Phase-0 thresholds
first, since Phase 1 inherits every one of them.
