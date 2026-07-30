# A/B methodology red-team (fable-5)

An adversarial methodology review of the 3-arm withholding A/B. Verdict up front: **the run is
invalid and cannot support any claim.** Findings, most severe first.

### 0. Total ceiling effect — no effect to explain (BLOCKING)
All 36 judgments are `true`; control = treatment = oracle = 100%. There is no contrast to
estimate. Worse, the prior `ab_score.py` printed a fixed "retrieval is the bottleneck" narrative
**regardless of the numbers** — a conclusion invariant to its inputs. *(Fixed: the scorer is now
data-conditional and reports INCONCLUSIVE.)*

### 1. Construct validity (BLOCKING/major)
- The 12 target decisions are **canonical pretraining knowledge** (float-epsilon comparison,
  route-pattern metric labels, `sql.NullTime`, typed enums, pinned Docker tags, `errors.Is`, no
  `os.Getenv` in constructors, tz columns). A frontier model complies from priors → the ceiling.
  The corpus's genuinely org-specific decisions (the real value) were not tested. No documented
  selection procedure → cherry-picking toward known practices can't be ruled out.
- Several task prompts **leak the answer** ("the `deleted_at` column is a nullable TIMESTAMP",
  "a large 19-digit integer", "correct wall-clock time for the user who created it", credentials
  "come from the service's configuration", "the only supported resource types are…").
- Some criteria are **un-triggerable or satisfiable by omission** (e.g. "does not require the
  caller to pass a logger" passes vacuously if logging is never mentioned).

### 2. Oracle arm is circular (major)
The criterion paraphrases `target_what`, and the oracle is handed that text → oracle compliance
measures "does the model echo a rule it was just told," i.e. instruction-following, not surfacing
value. Report oracle as a manipulation check, not a value estimate; use executable checks instead.

### 3. Judge validity (BLOCKING for future runs)
Single unvalidated LLM judge; **36/36 `true`, no rationales, no negative control, no second judge,
no human sample**. n-gram forensics: control shares 0 four-grams with the target text, treatment
20 and oracle 16 → the anonymized answers **leak their arm stylistically**; A/B/C shuffling doesn't
hide it. Same model family authored scenarios, all three arms, and the judgments in one 8-minute
session → monoculture / self-enhancement bias. Position assignment is also imbalanced.

### 4. Demand characteristics / contamination (BLOCKING for the treatment arm)
Treatment/oracle agents were told to consult/follow memory — that instruction alone lifts
conformance, and there is no **placebo arm** to separate instruction from content. Prompts were not
logged (unauditable). And treatment answers **verbatim-echo target text that `/recall` returned in
0/12 cases** ("the matched route pattern", "breaks the contract and") → leakage through the
authoring session, so the treatment arm is not what the design claims.

### 5. Retrieval 0/12 is a worst-case lower bound (BLOCKING for the "retrieval is the bottleneck" claim)
Exact-decision-ID matching over a **redundant corpus** (58 tz/UTC, 12 `latest`, 7 float/epsilon
decisions) counts a semantically-equivalent neighbor as a miss. No query/return-ID logs; the
`retrieve.top_k`/floor config for the treatment run is unrecorded. The 82%→0% cliff vs the
question-framed set is measured under adversarial authorship + strict matching + unknown config at
once, so it cannot be attributed to task-framing alone.

### 6. Statistical validity (BLOCKING for any quantitative claim)
N=12, single run, no CIs/tests. 12/12 → Wilson 95% CI [75.8%, 100%]; a difference of two
proportions at n=12/arm has 95% half-width ≈ ±40 pp. Only existence proofs are licensed — and this
run has zero saves.

### 7. Other
Scorer robustness bugs; no prompt logs → the core experiment is not reproducible from the repo.

## What a valid v2 requires
Org-idiosyncratic decisions a model can't know a priori, with a no-memory pilot confirming control
**fails** (~30–60%); temptation-neutral task phrasing; **executable** compliance checks (not an LLM
judge) or ≥2 validated judges from different families with rationales + Cohen's κ; a **placebo arm**;
**logged** `/recall` queries + returned IDs scored for semantic equivalence; and **N≈50–100** for
Wilson intervals narrower than ±20 pp.

## The one legitimate finding
On well-known engineering conventions, org-memory is **redundant with model priors** — its value
must be demonstrated on decisions the model *cannot* know a priori. That is the experiment worth
building.
