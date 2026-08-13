# Ranking + Robustness Regression Validation

Target: http://localhost:8000  •  Date: 2026-07-30  •  Mode: deterministic (HTTP GET/POST + jq)

Overall: ~~**5/5 PASS** — no regressions.~~ **REVISED 2026-07-30** → **3/5 PASS, 1 vacuous (now
re-run), 1 overstated (now split)**. `rank.w_authority` restored to `0.15` after test.

> **CORRECTED 2026-07-30** (META_AUDIT corrections #4 and #5). Check 1's two authority sub-claims
> were **vacuously true** — they could not have failed — and Check 4 rested on a single query pair.
> The underlying defect has since been fixed in `rank.go` and both checks re-run against the fixed
> build. Details in-place below.

---

## Check 1 — Relevance-primary ranking (bounded prior): ~~PASS~~ → **PASS (property now genuinely tested; authority confirmed inert in practice)**

> **What was wrong.** `rankAndFilter` computed `prior = Σ wᵢxᵢ` and then clamped that **sum** at
> `prior_cap` (0.01). Recency + importance + retention alone always exceed 0.01, so the sum
> **saturated for every candidate** and every item received exactly `+prior_cap`. Measured on the
> pre-fix build: `score − similarity = +0.010` for every item in every provenance — a CTO-authored
> review and an unattributed book alike.
>
> A uniform offset cannot reorder anything. So the "eviction guard" below could not have failed (a
> `author:vikash` item receives no differential boost, so eviction was never possible) and the
> "authority toggle" could not have failed either (a constant offset is invariant under
> re-weighting). Both were reported as evidence that a *bounded authority prior* behaves safely.
> They were evidence of nothing. The report's own numbers show the tell — `0.770 → 0.780`,
> `0.819 → 0.829`, `0.775 → 0.785`, a `+0.010` delta in every row — without remarking on it.
>
> **Fixed** in `rank.go`: the prior is now a weighted **mean** of `[0,1]`-scaled signals, scaled by
> `prior_cap` — bounded by construction rather than by clamping a runaway sum (score normalization
> before linear combination: Lee, SIGIR 1997; Montague & Aslam, CIKM 2001). Regression tests
> `TestRankPriorIsNotConstant` / `TestRankAuthorityToggleChangesOrdering` fail against the old code.

**Re-run on the fixed build (20 queries, top-5, `w_authority` 0 vs 0.15):**

| Measure | Pre-fix | Post-fix |
|---|---|---|
| `score − similarity` spread | +0.010 for **every** item (constant) | 0.004–0.005 (varies by item) |
| top-5 ordering changes when toggling `w_authority` | 0/20 (**impossible** by construction) | **0/20** (possible, but did not occur) |
| top-1 changes | 0/20 (impossible) | 0/20 |

**The honest conclusion is not the one the original check implied.** The prior now differentiates —
in isolation, authority orders CTO > unknown-author > unattributed exactly as designed, and the unit
tests prove it. But at the deployed weights it still cannot change any real outcome:

```
max authority contribution = prior_cap × w_authority / Σw = 0.01 × 0.15/1.35 = 0.00111
adjacent cosine gaps in top-5 (n=35): median 0.0110, p10 0.0010, p90 0.0450
→ authority alone could close 5/35 observed gaps; the whole prior could close 15/35
```

Authority's maximum influence is **10× smaller than the median gap it would need to close**. So:
**Check 1 passes, and the authority prior is bounded and safe — but it is also, in practice, inert.**
It is a formality at these weights, not a working seniority signal. That is a design decision to
make deliberately, not a property to claim as validated. If authority is meant to actually decide
near-ties, `prior_cap` would need to be ≈0.05 (at which point authority could close 10/35 gaps and
the whole prior 33/35) — with a correspondingly larger risk of burying a more-relevant answer, which
is exactly the failure the cap exists to prevent. **Not changed here; flagged for a decision.**

### Original Check 1 evidence (retained — the observations are reproducible, the inferences were not)

For each query the highest-COSINE on-topic decision ranks #1, and a higher-authority
(`author:vikash`) but lower-cosine item does NOT evict the more-relevant one.

| Query | #1 result (what) | #1 sim | #1 score |
|---|---|---|---|
| how should we do logging levels | "Set logging verbosity with the LOG_LEVEL env var…" | 0.770 | 0.780 |
| environment variable configuration in gofr | "Configure GoFr via environment variables in a configs/…" | 0.819 | 0.829 |
| how do I write custom middleware in gofr | "In GoFr, write middleware with the standard net/http…" | 0.775 | 0.785 |

- In every query the #1 item is the max-cosine item and results are in strict
  descending cosine order.
- **Eviction guard:** unfiltered `logging levels` top-3 are all `gofr-docs` items
  (sim 0.770 / 0.759 / 0.756). The `author:vikash` items on the same topic sit at
  sim ~0.754 and do NOT appear in the top-3 — the +0.15 authority weight does not
  lift a lower-cosine higher-authority item above a materially-more-relevant one
  (even the 3rd docs item at 0.756 out-ranks the best vikash item at 0.754).

**Authority toggle (ordering stability):** POST `/config {"rank.w_authority":"0"}` vs `"0.15"`.
Ordering (by decision id) is IDENTICAL across both settings for all three queries:

- logging: d-554edf1aa4f0 → d-aa94c3193d82 → d-5f86fdcf49fa (both w=0 and w=0.15)
- env vars: d-bd6974b133f6 → d-dcdb9e40a4b5 → d-f55451be5844
- middleware: d-8c88ecf3697d → d-0084e3817ffb → d-53751a58e534

Ordering is stable and relevance-led. Restored to `0.15`.

## Check 2 — Hard facet filter: PASS

- `q=… &author=vikash`: every returned item has `author:vikash` in its scope
  (e.g. d-413aec6d2323, scope `["repo:gofr","author:vikash","kind:review"]`).
  No non-vikash items leak through.
- `q=… &scope=kind:book`: every returned item has `kind:book` in its scope
  (e.g. `book:100-go-mistakes`, `book:clean-code`, `book:ai-engineering`).
  No non-book items leak through.

## Check 3 — Relevant surfaces / irrelevant abstains: PASS

- `q=environment variable configuration in gofr` → count 3, weak=false (surfaces hits).
- `q=what is the capital of France` → **count 0**, ~~top_similarity 0~~ (clean abstain).
  **Corrected:** the abstention is correct, but `top_similarity 0` is an absence, not a measurement.
  The true best cosine was **0.523** against a 0.653 floor. See
  [`abstention_calibration.md`](abstention_calibration.md).

## Check 4 — Weak flag: ~~PASS~~ → **INCONCLUSIVE as run; defect found and fixed**

> **What was wrong** (META_AUDIT correction #5). Two queries cannot support a "PASS" on a
> confidence signal, and the one-pair result sat directly against
> [`abstention_calibration.md`](abstention_calibration.md)'s finding that `weak` fired on **16/32**
> genuine hits — including matches at similarity 0.84–0.85 — which the audit reproduced
> independently (weak=true on 5/5 genuine hits, up to 0.831). A flag that fires on half of all
> correct answers is not a working confidence signal, and this check's framing concealed that.

**Root cause.** `weak` OR-ed together two unrelated conditions:

- `absWeak` — the top match is barely above the floor (*absolute confidence*)
- `marginWeak` — the top match doesn't separate from its runner-up (*flat distribution*)

`marginWeak` fires constantly, because a good query legitimately matches several near-equally-relevant
decisions. That is **redundancy in the corpus, not low confidence in the answer** — and conflating
them produced a flag that was simultaneously over-cautious on genuine hits and silent on the
confidently-wrong negation failures it most needed to catch (`negation.md`: "should we fail closed"
returned "Fail **open**" at 0.778 with `weak=false`). A second, latent defect: `absWeak` compared
against the absolute `retrieve.precision_floor` rather than the **effective** floor the gate applied,
which silently mis-labels confidence whenever adaptive gating is doing the work.

**Fixed.** `recall` now reports the two signals separately, both measured against the effective
floor. Re-run on the fixed build:

| Query | sim | pre-fix `weak` | post-fix `weak` | post-fix `ambiguous` |
|---|---|---|---|---|
| graceful shutdown | 0.762 | false | false | false |
| circuit breaker | 0.753 | **true** | false | **true** |
| rate limiting | 0.784 | **true** | false | **true** |
| error handling conventions | 0.755 | **true** | false | **true** |
| should we fail open when redis is down | 0.804 | false | false | false |

Every previously-"weak" genuine hit is now correctly `weak=false, ambiguous=true` — the over-firing
was entirely the margin rule, as suspected. **This does not yet make `weak` a validated confidence
signal**: it makes it a *measurable* one. A real verdict needs a labeled set of correct and incorrect
recalls, which this validation does not have — the negation failures are the natural source, and
`weak` remains `false` on them.

## Check 5 — Auto-calibrated precision floor: PASS

- GET `/config?key=retrieve.precision_floor` → **0.6528** — present and in a sane
  range (0<floor<1, plausible cosine cutoff sitting just below the on-topic hit band ~0.74–0.82).

---

Final state verified: `rank.w_authority = 0.15` (restored, confirmed via `GET /config` after the
re-run).

## Revised scorecard

| Check | Original | Revised | Why |
|---|---|---|---|
| 1 — relevance-primary / bounded prior | PASS | **PASS** (re-run) | claims were vacuous pre-fix; prior now differentiates, but is inert at deployed weights — stated plainly rather than claimed as validated |
| 2 — hard facet filter | PASS | **PASS** | independently reproduced by the audit; unaffected |
| 3 — surfaces / abstains | PASS | **PASS** | correct, but the `top_similarity 0` figure was an absence, not a measurement |
| 4 — weak flag | PASS | **INCONCLUSIVE** | one query pair; defect found, fixed, re-measured — still needs a labeled set to earn a verdict |
| 5 — precision floor | PASS | **PASS** | 0.6528 confirmed by the audit |

**Open design question raised by this re-run** (not a defect — a choice): at `prior_cap = 0.01` the
authority prior cannot influence any observed ranking. Either accept that relevance is effectively
absolute and stop describing authority weighting as an active ranking signal, or raise `prior_cap`
and re-validate the burial risk the cap was introduced to prevent.
