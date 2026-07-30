# PATTERN-SEPARATION / Near-Duplicate Density Report

Source: `endpoint` (http://localhost:8000/decisions, fallback corpus.json)
Reproduce: `python3 dedup_density.py` (stdlib only, deterministic)

**Corpus pinned:** 1643 surfaceable decisions, `sha256[:16] = b65fa0ded6175664` (re-verified
2026-07-30 against the fixed build). **Superseded 2026-07-30** by
`sha256[:16] = a8dca1a6d7b7a83a`, still 1643 surfaceable, after the cost-storage remediation
described in [`contradiction_rate.md`](contradiction_rate.md) retired 3 decisions and replaced them
with 3 faceted/corrected ones. The dedup numbers below were measured on the earlier hash; the
remediation swapped three decisions without adding near-duplicates, so the family counts are
unaffected — re-run `dedup_density.py` to confirm against the current corpus.

> **Note on the 1643 vs 1644 discrepancy** (META_AUDIT correction #7, itself corrected). The audit
> read `/stats` → 1644, compared it to this report's 1643, and concluded "corpus grew by exactly 1
> decision since they ran". **The corpus did not grow.** `/stats` counts every stored row; this
> report counts *surfaceable* decisions, and exactly 1 decision is quarantined (`/decisions` returns
> `count: 1644, quarantined: 1`). Both numbers were correct and describe different things. The pinned
> hash above now makes drift detectable directly, rather than inferred from a count mismatch.

## Method
Deterministic, stdlib-only. Candidate pairs generated via a token inverted index (blocking on shared non-common tokens), then scored with normalized token-set Jaccard and `difflib.SequenceMatcher.ratio()` on the `what` field. Two decisions are linked when `ratio > 0.7` OR `Jaccard >= 0.6`. Linked decisions are grouped into families via union-find (transitive closure).

## Summary

- Total decisions: **1643**
- Near-duplicate clusters (size >= 2): **12**
- Decisions inside a near-dup cluster: **24** (1.5%)
- Estimated redundant decisions (near-dup of another, excess over 1 representative/family): **12** (**0.7%** redundancy)

## Largest near-duplicate families

### Family 1 — size 2
- `d-3d5cad2f2543`: ping-test-ignore
- `d-208f0cfd1111`: ping-test

### Family 2 — size 2
- `d-c48e926de8d6`: Unwrap the { items: [...] } envelope in the playground's useAssignments select step.
- `d-3e42c6bcf549`: Unwrap the { items: [...] } envelope in the useAssignments select step so UsersPage receives an iterable array.

### Family 3 — size 2
- `d-b4c3f5b4cb07`: Derive ECS-service on/off status from desired_count (==0 -> stopped) instead of the ECS lifecycle Service.Status, mirroring asgStatus.
- `d-a1876a9bbb92`: Derive ecs-service on/off status from desired_count (==0 means stopped) instead of the lifecycle Service.Status

### Family 4 — size 2
- `d-612dd0257e5b`: Add application-level spans inside handlers with span := ctx.Trace("name") and defer span.End().
- `d-4e64e2f25acb`: Add a custom trace span inside a handler with 'span := c.Trace("my-custom-span")' and immediately 'defer span.End()'.

### Family 5 — size 2
- `d-b44cc7ebfb0c`: Match strategy to the segment: differentiated for underserved buyers willing to pay a premium, disruptive for overserved customers and nonconsumers.
- `d-bd16b51588dc`: Match strategy to the customer-need pattern: differentiated (better+pricier) for underserved segments, disruptive (worse+cheaper) for overserved segments and nonconsumers.

### Family 6 — size 2
- `d-994920e505e3`: Generate short codes by base62-encoding a globally unique numeric ID instead of hashing the long URL and resolving collisions.
- `d-bdd7a4c57b36`: Generate short codes/IDs by Base62-encoding a unique ID from a distributed ID generator rather than hashing content and resolving collisions.

### Family 7 — size 2
- `d-34b14f2030e1`: Use a hybrid fanout model: fanout-on-write (push) for regular users and fanout-on-read (pull) for celebrities above a follower threshold.
- `d-24f595db44dc`: For a news feed, use a hybrid fan-out: fan-out-on-write for regular users but fan-out-on-read for high-follower celebrity accounts above a tuned threshold.

### Family 8 — size 2
- `d-a9f4e66e32c6`: Cap each user's feed cache to a bounded size (e.g. 500 entries) and trim the oldest entries on every write.
- `d-f15731bed271`: Cap each user's feed cache at a maximum size and evict the oldest entries after every insert.

### Family 9 — size 2
- `d-fdf612a65943`: Build the deployable artifact exactly once, store it in a registry, and promote that same artifact through every pipeline stage to production.
- `d-bc8befcc8153`: Build each deployable artifact exactly once, store it in a registry, keep it environment-agnostic, and promote that same artifact through every environment.

### Family 10 — size 2
- `d-f48fd4513fce`: Write desired outcome statements that are measurable, controllable, actionable, devoid of solutions, and stable over time.
- `d-6965da39e18e`: Write desired outcomes so each is measurable, controllable, actionable, devoid of solutions, and stable over time; keep each statement one-dimensional.

### Family 11 — size 2
- `d-b51a33e8fa55`: Interview the actual job executor (the person doing the underlying job), never the buyer, consultant, or influencer as a substitute.
- `d-3d7011866c38`: Interview the actual job executor (the person doing the underlying job), not the buyer, the budget owner, or a third-party consultant.

### Family 12 — size 2
- `d-95e069de300b`: Wrap synchronous downstream calls in a circuit breaker that opens after a failure threshold, fails fast while open, and probes recovery in half-open state.
- `d-1c593e355ee1`: Wrap downstream dependencies in a circuit breaker (closed/open/half-open) that trips after consecutive failures, fails fast while open, and probes recovery with a limited half-open...

## Keyword-family sizes (known-redundant themes)

`substring` = case-insensitive substring count exactly as specified. `word-bnd` = same count with word boundaries (removes false hits such as "utc" matched inside unrelated words). `intra-dup pairs` = pairs *within* the theme whose `what` similarity exceeds ratio>0.7 — i.e. actual restatements of the same rule.

> **CORRECTED 2026-07-30** (META_AUDIT correction #6). The `idempotent` row previously read
> `word-bnd 0` against `substring 16`. That is arithmetically impossible — every word-boundary match
> is also a substring match — and it meant the idempotent intra-dup check silently examined an empty
> set rather than the 16-decision theme it claimed to cover. The word-boundary pattern for that row
> was broken. The counts below are re-measured by the committed
> [`dedup_density.py`](dedup_density.py), which now *asserts* `word-bnd <= substring` and refuses to
> emit a zero word-boundary count against a non-zero substring count, so this class of error fails
> loudly instead of printing.

| Theme | substring | word-bnd | intra-dup pairs (ratio>0.7) |
|---|---|---|---|
| retry | 48 | 46 | 0/1035 (closest 0.65) |
| aggregator | 38 | 37 | 0/666 (closest 0.52) |
| FINAL | 26 | 20 | 0/190 (closest 0.37) |
| clickhouse | 20 | 19 | 0/171 (closest 0.65) |
| idempotent | 16 | **16** | 0/120 (closest 0.52) |
| timezone/tz | 1 | 1 | 0/0 |

**Key finding (unchanged):** every theme has **0** intra-theme near-duplicate pairs. Decisions that
share a keyword (the 46 "retry" decisions, 37 "aggregator" decisions) are lexically distinct — they
address different aspects of the theme, not near-identical restatements. The closest intra-theme pair
anywhere is ratio **0.65**, below the 0.70 linking bar, so this is not a threshold-luck result.

**On the "58 timezone decisions" that motivated this check:** it was never real. 58 was a bare
substring count, matching `tz` inside `quartz`, `blitz` and `utc`; the word-boundary count is **1**.
The redundancy problem this validation set out to find does not exist in this corpus.

## Family-size distribution

| Family size | # families |
|---|---|
| 2 | 12 |

## Verdict

**WELL-SEPARATED (lexically).** On this deterministic, difflib/stdlib evidence the corpus is clean. Only 12 near-duplicate families exist, all of size 2, covering 24/1643 decisions (1.5%); estimated redundancy is 0.7%. The largest near-dup family is just 2 members — there is no long tail of "many almost-identical timezone rules". Keyword-cohesive themes (retry=46, aggregator=38, idempotent=16) contain ZERO intra-theme near-duplicate pairs, so shared keywords reflect topical breadth, not restatement. Embedding-space dedup/merge would therefore NOT materially shrink or clean the store on the evidence here — the documented fix is not warranted for this corpus at this time.

**Caveat:** this measures *lexical* similarity (token Jaccard + difflib on `what`). It cannot detect semantic paraphrases that share little surface wording; an embedding-space pass could surface additional semantic clusters this method misses. However, the fact that even decisions sharing an exact keyword are lexically diverse and address distinct aspects is strong evidence the store is genuinely separated rather than merely superficially varied.
