# META-AUDIT (cross-family, fable) — of the 5 validation reports

**Auditor model family:** fable (cross-family check)
**Date:** 2026-07-30
**Live system:** http://localhost:8000, confirmed up. `/stats`: decisions **1644** at audit time (reports say 1643 — corpus grew by exactly 1 decision since they ran; immaterial). `/config?key=retrieve.precision_floor` = **0.6528**, `rank.w_authority` = **0.15**.
**Method:** for each report I extracted its load-bearing claims and re-tested them against the live service with **my own queries** (not the validators'), then judged SOUND / OVERSTATED / WRONG / UNVERIFIABLE.

---

## Overall verdict

The five validations are **substantially trustworthy on their empirical findings** — every headline phenomenon reproduced under fresh queries — but **two reports contain claims that must be corrected before publication**. The most serious error is in `contradiction_rate.md`: its root-cause claim ("there is no supersede mechanism at all; `supersedes`/`superseded_by` absent from the schema, verified against raw `/decisions`") is **factually false** — the schema has a `supersedes` field, 3 decisions use it, and superseded decisions are actually hidden from the store view. The contradiction itself, however, is real and worse than easy to dismiss: I reproduced it with brand-new queries. `ranking_robustness.md` reports two checks as meaningful passes that are in fact **vacuously true** — the authority component adds a uniform +0.010 to every item regardless of provenance, so the "eviction guard" and "w_authority toggle" checks could never have failed. Abstention, negation, and dedup reports all held up well, with one masked-metric framing issue and one broken table row noted below.

---

## Summary table

| Report | Validator's load-bearing claim | My independent finding | Verdict |
|---|---|---|---|
| abstention_calibration.md | 0/32 false-abstain, 0/32 false-inject; off-topic → top_similarity 0.0; weak over-fires (16/32 genuine hits) | 6/6 fresh off-topic queries abstained (count 0, top_sim 0, weak false); 5/5 fresh in-corpus queries hit (sim 0.679–0.831); weak=true on 5/5 genuine hits incl. sim 0.831. BUT top_similarity=0.0 is a **masked** value, not a measured margin; and a near-domain query ("react useEffect cleanup") **did** inject at sim 0.658 | **SOUND** (one framing correction) |
| contradiction_rate.md | 3/32 (9.4%) strict contradictions, all in order-storage cluster; root cause = **no supersede/versioning field exists** | Cluster fully reproduced with fresh queries: MySQL-SoT + CH-sole-store co-occur (0.723/0.706); dual-write + no-dual-write co-occur top-2 (0.788/0.737). BUT the schema **has** `supersedes`; 3 decisions populate it; superseded ids are absent from `/decisions` (mechanism works) | **OVERSTATED** (empirics right, root cause wrong) |
| negation.md | 9/16 identical top across polarity; 0/16 respect direction; 2/9 failures weak:false | 1/5 of my pairs identical-top, but that one ("fail open" vs "fail closed") returned the **same** "Fail open" decision with **weak:false on both sides**; "do not retry" scored the pro-retry decision **higher** (0.749) than the positive query (0.709); 0/5 of my NEG queries retrieved opposite-direction content | **SOUND** |
| dedup_density.md | 12 near-dup families, all size 2; ~0.7% redundancy; 0 intra-theme dup pairs in retry/aggregator/FINAL | My independent recompute: **13 families, all size 2** (corpus +1, thresholds slightly looser), same top families verbatim; retry intra-dup pairs = 0 (closest ratio 0.65); aggregator 38/37 and FINAL 26/20 match exactly. One broken row: "idempotent word-bnd = 0" is impossible (real count 13–16) | **SOUND** (one buggy row) |
| ranking_robustness.md | 5/5 PASS: cosine-primary order; vikash never evicts; ordering identical at w_authority 0 vs 0.15; facets pure; France abstains; floor 0.6528 | France abstains ✓; floor 0.6528 ✓; facet purity ✓ on my own author=vikash and scope=kind:book queries; strict descending-sim order ✓. BUT score−sim = **+0.010 for every item in every provenance** (docs, vikash, books) — the authority prior is a constant offset, so the eviction and toggle checks were **vacuous**, not evidence of a bounded prior | **OVERSTATED** |

---

## 1. abstention_calibration.md — SOUND (with one correction)

**Claims re-tested:** (a) off-topic queries abstain with top_similarity 0.0; (b) answerable queries always return hits in the ~0.69–0.89 band; (c) weak over-fires on genuine hits.

**My probes (all my own queries):**
- Off-topic ×6: `who won the 2018 fifa world cup`, `best chocolate chip cookie recipe`, `how tall is mount everest`, `tips for growing roses in winter`, gibberish `zxqv flurble womp qwerty`, `what is the capital of France` → **all** `count 0, top_similarity 0, weak false`. 6/6 clean abstain. Matches 0/32 false-inject in direction.
- In-corpus ×5: graceful shutdown (0.831), gateway tenancy (0.679), circuit breaker (0.791), rate limiting (0.796), metrics port (0.802) → **all** returned 3 hits. Matches 0/32 false-abstain. Their claimed sim band (min 0.694, max 0.885) is consistent with mine.
- Weak over-firing: **weak=true on all 5** of my genuine hits, including graceful-shutdown at sim **0.831** — confirms (and if anything strengthens) their "weak fires on strong matches" finding; meanwhile a sharp `should we fail open when redis is down` returned weak=false at 0.804, confirming non-monotonicity.

**Where the report is wrong / must be softened:**
- "The margin between the weakest real hit (0.694) and the strongest off-topic (0.000) is enormous" — **this margin is an artifact.** `top_similarity: 0` is a masked/zeroed value emitted when nothing clears the precision floor, not a measured cosine. Proof: `kubernetes ingress nginx annotations` returns top_sim 0 against a corpus that demonstrably contains Helm/K8s deployment decisions — its true best cosine cannot be 0.0. The gate works (count 0 is correct), but no similarity-separation conclusion can be drawn from the 0.000 figures.
- Their own caveat about near-domain negatives is vindicated: my `react useEffect cleanup best practices` query **injected 2 results** (top sim 0.658, an unrelated `useAssignments` envelope decision, weak=true). So "0% false-inject" holds only for far-off-topic negatives; the boundary case fails softly (weak=true) but does inject.

**Verdict: SOUND.** Headline confusion matrix reproduces perfectly in direction; the "similarity separation" section needs rewording.

## 2. contradiction_rate.md — OVERSTATED (empirics confirmed; root-cause claim false)

**Claims re-tested:** (a) the three order-storage decisions surface together contradictorily; (b) 9.4% strict rate plausible; (c) "no supersede mechanism at all — no `supersedes`/`superseded_by` field in the schema, only `quarantined`" (their words: "verified against raw `/decisions` records").

**My probes (my own phrasings, not theirs):**
- `which database is the source of truth for order data` → top-3: `d-cd39778fbe0c` (0.723, "MySQL … is the source of truth; ClickHouse … best-effort dual-write replicas") **and** `d-cbfde40003b1` (0.706, "ClickHouse is the sole store for orders — no MySQL dual-write") side by side. Strict contradiction reproduced.
- `does the aggregator dual write metrics to clickhouse` → top-2: `d-05d7cb7f3b50` (0.788, "Dual-write … to both MySQL and ClickHouse") + `d-cbfde40003b1` (0.737, "no MySQL dual-write"). Reproduced again with a completely different query.
- 2 of my 4 cost-adjacent queries surfaced a strict contradictory pair; their 3/32 (9.4%) over a mixed-topic set is plausible, arguably conservative for order-storage-adjacent traffic.
- Their timeline detail checks out: cbfd created 08:01:59, cd39 08:02:32, 05d7 08:04:02 (2026-07-29), all `quarantined:false`, all `supersedes:null`.

**Where the report is WRONG:**
- The raw decision schema **contains a `supersedes` field**: `['created','id','provenance','quarantined','scope','source','supersedes','updated','what','why']`. It is populated on **3 of 1644** decisions (`d-c6c818970276`, `d-ad52649fca21` — REDTEAM cleanup tombstones — and `d-3b356b480043` superseding `d-ee39533c18ae`). The three superseded target ids are **absent** from `/decisions` entirely, i.e. the supersede mechanism exists **and is honored** by the store/recall view. The report's root-cause paragraph and its "verified against raw /decisions" assertion are false; the correct root cause is that the order-storage cluster's `supersedes` fields were **never populated**, not that the primitive is missing.
- Consequently the recommendation "add an explicit supersede edge (or status field)" should read "populate the existing `supersedes` field for `d-05d7cb7f3b50`/`d-cd39778fbe0c`/`d-cbfde40003b1`."

**Verdict: OVERSTATED.** The dangerous-contradiction finding is real and robust (I'd co-sign it), but a load-bearing "verified" schema claim is factually wrong and flips the remediation story.

## 3. negation.md — SOUND

**Claims re-tested:** (a) opposite-intent queries return the identical positive-direction top decision; (b) some failures are served weak:false; (c) negated queries sometimes score *higher*; (d) direction is never correctly respected.

**My probes (5 pairs of my own, none reused from the report):**

| Pair | POS top (sim/weak) | NEG top (sim/weak) | Outcome |
|---|---|---|---|
| enable / disable rate limiting | d-5a528b127056 0.734/T | d-7b0989c26273 0.721/T | drift (both pro-rate-limiting) |
| add / drop database index | d-707a93ed33ff 0.718/T | d-faa09f224b83 0.675/T | drift |
| fail **open** / fail **closed** when redis down | d-e88c753446c5 "Fail open…" 0.804/**F** | **same** d-e88c753446c5 0.778/**F** | **identical top, weak:false both sides** |
| add retries / **do not** retry failed requests | d-e88c753446c5 0.709/T | d-3370239d0279 "Retry failed calls with exponential backoff…" **0.749**/T | NEG scored pro-retry advice **higher** than POS scored its top |
| turn on / turn off request logging | d-ba33f78b1976 0.664/T | abstained (count 0) | abstain |

- The fail-open/fail-closed pair is a textbook reproduction of their worst case: asking "should we fail **closed**" returns "Fail **open** (allow the request)" at 0.778 with **weak:false** — confidently wrong, exactly their "2 of 9 failures weak:false" pattern.
- "do not retry" retrieving the pro-retry decision at 0.749 (higher than the positive query's 0.709 top) reproduces their #6/#7 "negation raises confidence" finding.
- **0/5** of my negated queries retrieved genuinely opposite-direction content — matches their 0/16.
- My identical-top rate (1/5) is below their 9/16, but that is pair-composition-dependent (my pairs drifted or abstained more); the load-bearing conclusion — retrieval is topic-driven and negation-blind, with the weak flag not a reliable guard — is fully confirmed.

**Verdict: SOUND.**

## 4. dedup_density.md — SOUND (one buggy table row)

**Claims re-tested:** (a) ~12 near-dup families, all size 2, ≈0.7% redundancy; (b) hot themes have 0 intra-theme near-dup pairs; (c) theme sizes.

**My probes:** I pulled `/decisions` (1644) and independently reimplemented the method (token-Jaccard ≥0.6 OR difflib ratio >0.7 on `what`, inverted-index blocking, union-find):
- **13 families, all size 2, 26 decisions** — vs their 12/24 on a corpus one decision smaller and with my slightly looser Jaccard-gated ratio path. Same top families verbatim (ping-test pair, useAssignments-envelope pair, ECS desired_count pair, trace-span pair, strategy-segment pair). Redundancy ≈0.8% vs their 0.7%. Reproduces within noise.
- **Retry theme: 0 intra-dup pairs** at their threshold; the closest pair in the whole theme is ratio **0.65** (`d-c36e8c684ed3`/`d-5ead447ec64e`) — below the 0.7 bar, so their "0" is not threshold-luck by much but it is correct.
- Theme counts: on `what`+`why` I get aggregator **38 substring / 37 word-boundary** and FINAL **26/20** — **exact matches** to their table (confirms they counted over what+why). Retry with inflections (retry|retries|retried|retrying) word-boundary = **46**, matching their headline "retry=46".
- **Error found:** their row "idempotent | substring 16 | word-bnd 0". A word-boundary count of 0 is impossible — `\bidempotent\b` (case-insensitive) matches **13** decisions, `\bidempoten\w*\b` matches **16**. Their word-boundary implementation broke on this row (and therefore their idempotent intra-dup check examined 0 pairs). Harmless to the verdict — my own check across the 16-decision idempotent set is subsumed by the global family scan, which found no idempotent family — but the row is wrong as printed.

**Verdict: SOUND.** The "well-separated, dedup not warranted" conclusion stands on independent recomputation.

## 5. ranking_robustness.md — OVERSTATED

**Claims re-tested:** (a) highest-cosine ranks #1, descending order; (b) author:vikash never evicts more-relevant items; (c) ordering identical at w_authority 0 vs 0.15; (d) facet purity; (e) France abstains; (f) precision_floor ≈0.6528.

**My probes:**
- France → `count 0, top_sim 0`. Confirmed. `/config?key=retrieve.precision_floor` → **0.6528** exactly. Confirmed. `rank.w_authority` is **0.15** now — their claimed restore did happen.
- Facet purity, my own queries: `error handling conventions&author=vikash` → 3/3 items carry `author:vikash` in scope; `caching strategy for read heavy workloads&scope=kind:book` → 3/3 `kind:book`. No leakage. Confirmed.
- Ordering: every result list I pulled (log levels, middleware, caching, fanout) is in strict descending cosine, and rank order = cosine order. Confirmed.
- **The problem:** across every provenance I sampled — `gofr-docs` (0.772→0.782), `author:vikash` reviews (0.745→0.755), `book:system-design-interview` (0.723→0.733) — **score − similarity = +0.010 for every single item**. With w_authority=0.15, the authority term contributes an identical constant to all items. Therefore:
  - "higher-authority vikash item does NOT evict more-relevant docs item" is **vacuously true** — vikash items receive zero differential boost, so eviction was never possible;
  - "ordering identical at w_authority 0 vs 0.15" is **guaranteed by construction** — a uniform offset cannot reorder anything.
  The validator presented both as evidence that a *bounded authority prior* behaves safely. In the deployed state there is **no effective authority prior to bound** — the checks pass but demonstrate nothing about the property named. Their own numbers show the same +0.010 delta (0.770/0.780 etc.) without noticing it.
- Check 4 (weak calibration) passed on their single vague/sharp pair and on mine (`fail open` sharp → weak:false 0.804), but presenting one pair as "Weak flag: PASS" sits badly next to the abstention report's (correct, and my-confirmed) finding that weak fires on 5/5 genuine hits up to sim 0.831. A one-pair PASS overstates the flag's health.

**Verdict: OVERSTATED.** All literal observations are reproducible and "no regressions" is fair, but Checks 1's two authority sub-claims are vacuous as evidence and must not be cited as proof the authority prior is well-bounded.

**Config note:** I changed no configuration; `rank.w_authority` read 0.15 before and after my probes.

---

## Corrections needed before the paper / validation plan

1. **contradiction_rate.md — root cause (MUST FIX):** delete "there is no supersede mechanism at all / `supersedes` absent from schema (verified)". The schema has `supersedes`; 3 decisions use it; superseded targets are hidden from `/decisions`. Rewrite root cause as "the supersede field exists and is honored but was never populated for the order-storage cluster"; rewrite the recommendation from "add a supersede edge" to "populate `supersedes` on the stale order-storage decisions."
2. **abstention_calibration.md — similarity-separation framing (FIX):** `top_similarity: 0.000` on abstentions is a post-floor masked value, not a measured cosine (a Kubernetes query cannot truly score 0.0 against a corpus containing Helm decisions). Remove/reword the "enormous margin between 0.694 and 0.000" claim; the abstention conclusion itself stands on `count==0`.
3. **abstention_calibration.md — scope of 0% false-inject (CLARIFY):** near-domain uncovered topics can inject (my react-hooks query returned 2 items at 0.658, weak=true). The report's caveat should be promoted from footnote to an explicit scope limit on the 0/32 figure.
4. **ranking_robustness.md — authority checks (MUST FIX):** state that the authority component currently contributes a uniform +0.010 to all items across provenances, so the eviction-guard and w_authority-toggle checks are vacuous; either fix/differentiate authority scoring and re-run, or downgrade Check 1 from "PASS (bounded prior safe)" to "PASS (inert prior — property untested)."
5. **ranking_robustness.md — Check 4 (SOFTEN):** a single vague/sharp pair cannot support "Weak flag: PASS" given the demonstrated over-firing (weak=true at sim 0.83); cross-reference the abstention report's weak-flag findings.
6. **dedup_density.md — idempotent row (FIX):** "word-bnd = 0" is a counting bug; the true word-boundary count is 13 (exact form) / 16 (inflections). Conclusion unaffected.
7. **Corpus count drift (NOTE):** corpus is now 1644 (+1 vs all five reports' 1643); harmless, but pin a corpus snapshot/hash for the paper's final numbers.

---

# ADDENDUM — resolution of the 7 corrections (2026-07-30)

All seven corrections were worked. Two of them turned out to be **wrong in the audit itself**, and
one produced a finding materially different from what either the validator or the auditor expected.
Recorded here rather than silently absorbed, because the point of a cross-family audit is defeated
if the audit's own errors are the ones that survive.

## Where this audit was itself wrong

**A. Correction #1's remediation would have destroyed a true fact.** The audit correctly demolished
the "no supersede mechanism" root cause, then prescribed: *"populate `supersedes` on the stale
order-storage decisions."* Checked against the source documents those decisions were extracted from,
two of the three are **both true and about different tables**:

- `d-cbfde40003b1` (ClickHouse sole store for `orders`) — verbatim `docs/ORDERS-ARCHITECTURE.md:27`
- `d-cd39778fbe0c` (MySQL `order_events` is source of truth) — verbatim `docs/EVENTS-ARCHITECTURE.md:33`

They are not versions of one decision; they are decisions about `orders` and about
`order_events`. "Populate supersedes" resolves by recency and would have retired the
metrics decision as a stale copy of the cost one. The real defect is that **no decision in the
cluster carries a subject facet** — every one is scoped `["repo:acme"]` — so retrieval cannot
tell the two subjects apart. The genuinely defective decision is the third, `d-05d7cb7f3b50`, which
over-generalizes "dual-write cost/allocation/metrics" across subsystems and is contradicted for cost
by `docs/ORDERS-ARCHITECTURE.md:179`. Notably, `contradiction_rate.md`'s own method notes already
excluded different-table pairs — the rule was right and simply wasn't applied.

**B. Correction #7's "corpus grew by exactly 1 decision" is false — the corpus never grew.** 1644 is
every stored row (`/stats`); 1643 is *surfaceable* decisions, which is what the five reports counted.
Exactly one decision is quarantined: `/decisions` returns `count: 1644, quarantined: 1`. Both
numbers were always correct and describe different populations. The underlying advice — pin a hash —
was right and is done: `sha256[:16] = b65fa0ded6175664` over 1643 surfaceable decisions.

## Where the audit was right, and what it turned up

**C. The masked-metric finding (#2) was worth more than a rewording.** With `top_candidate_similarity`
now measured pre-gate, the abstention margin is not "enormous" but **0.073** — and the nearest
off-topic miss falls only **0.032** short of the floor. Random gibberish scores **0.612** against a
0.653 bar, and outscores a real English question about mountain heights. The gate holds; the
confidence in it was overstated by roughly an order of magnitude.

**D. The vacuous-authority finding (#4) was correct, and the fix reveals a second problem the audit
could not have seen.** The prior was a sum clamped at `prior_cap`, so it saturated to a constant
`+0.010` and carried no ordering information. That is fixed (normalize, then bound). But re-running
the toggle check on the fixed build still yields **0/20 ordering changes**, because authority's
maximum influence is `0.01 × 0.15/1.35 = 0.00111` against a **median adjacent cosine gap of 0.0110**
— 10× too small to matter. The prior is no longer degenerate; it remains *practically inert*. The
audit's proposed downgrade ("inert prior — property untested") was the right call, and is now backed
by measurement rather than inference.

## Disposition

| # | Correction | Status |
|---|---|---|
| 1 | contradiction root cause | **Fixed, and the audit's remediation replaced** — root cause rewritten against source docs; real cause is missing subject facets + a missing *verb* (`POST /supersede` added) |
| 2 | abstention margin framing | **Fixed at the source** — `top_candidate_similarity` + `floor` now returned; margins re-measured |
| 3 | false-inject scope limit | **Fixed** — promoted from footnote to an explicit scope limit on the 0/32 figure |
| 4 | vacuous authority checks | **Fixed and re-run** — `rank.go` normalizes before bounding; regression tests added; re-measured as still inert at deployed weights |
| 5 | Check 4 weak flag | **Fixed and re-run** — `weak` / `ambiguous` split; over-firing eliminated; verdict downgraded to INCONCLUSIVE pending a labeled set |
| 6 | dedup idempotent row | **Fixed** — true word-boundary count is 16; check committed as `dedup_density.py`, which now asserts the impossible case instead of printing it |
| 7 | corpus snapshot | **Done, and the drift claim corrected** — hash pinned; there was no drift |

## Corpus remediation — applied, and it produced a third correction

Applied 2026-07-30 with approval. Corpus moved from `sha256[:16] = b65fa0ded6175664` to
`a8dca1a6d7b7a83a` (1643 surfaceable both before and after; 3 decisions retired, 3 added). Backup
taken pre-change.

**Result: 1 of the 2 defects in the cluster is fixed.**

- ✅ The **genuine** contradiction is closed. `d-05d7cb7f3b50`'s over-generalized dual-write claim
  was replaced with one that names `orders` as the explicit exception. Across 8 order-storage
  queries, 0 retired decisions surface, and the pair that previously gave opposite advice now gives
  consistent advice.
- ❌ The **false** contradiction survives, and **the subject facets did not fix it** — which
  corrects finding (A) above, this addendum's own diagnosis. Two reasons compound: `table:` is not
  in `hardFilterPrefixes` (`["author:", "kind:"]`), so it is an inert soft prior — verified, since
  `scope=table:orders` and `scope=table:order_events` return identical sets — and
  even as a hard predicate it could not help a free-text query that carries no facet.

Missing subject facets were a *necessary* condition for the conflation, not a sufficient remedy. The
residual failure belongs to **retrieval**, not metadata: both decisions carry their table name in
the text and the bi-encoder still ranks "source of truth" phrasing above the distinguishing entity.
That is the same root cause as the negation blindness in `negation.md` and wants the same fix — a
cross-encoder re-ranker over the top-k, which would address entity and polarity together.

**Chain of corrections on this one cluster, for the record:** the validator diagnosed a missing
schema field (wrong); the audit diagnosed unpopulated fields and prescribed supersede-by-recency
(wrong, and would have destroyed a true decision); this addendum diagnosed missing subject facets
(necessary but insufficient); the applied remediation showed the residual cause is the retriever.
Each pass was closer, and none was reliable alone — which is the argument for executing a
remediation rather than reasoning about it.
