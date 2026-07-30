# Withholding A/B — results

> **Status: INCONCLUSIVE.** This is a pilot that FAILED to produce a usable measurement; see `AB_REDTEAM.md` for the full list of validity threats. The numbers below are reported for honesty, not as evidence for or against the system.

- scenarios: **12**
- retrieval@task (exact decision-ID match; a lower bound — semantic-equivalent hits count as misses): **0/12**

| arm | compliance | Wilson 95% CI |
|---|---|---|
| CONTROL (no memory) | 12/12 = 100% | [76%, 100%] |
| TREATMENT (live /recall) | 12/12 = 100% | [76%, 100%] |
| ORACLE (decision surfaced) | 12/12 = 100% | [76%, 100%] |

- saves (control✗→treatment✓): 0/12   |   oracle ceiling (control✗→oracle✓): 0/12

**INCONCLUSIVE — control ceiling.** The no-memory control complied on all 12/12 scenarios, so there is no room for any arm to improve and NO surfacing effect is estimable. This does not show the memory is useless; it shows the scenarios test conventions the base model already follows (redundant with model priors), so they cannot discriminate. A valid run needs scenarios where control demonstrably fails (~30–60%).

**Why it's inconclusive (red-team, summarized).** (1) Ceiling: control complied everywhere, so no contrast exists. (2) Construct: the sampled decisions are canonical best practices the base model knows from pretraining, and several task prompts leak the answer. (3) Judge: a single unvalidated LLM judge returned 36/36 compliant with no rationales and no negative control. (4) Retrieval 0/12 is exact-ID on a deliberately adversarial, worst-case task set with unlogged queries. (5) N=12 → Wilson CIs ≈ ±25–40 pp; no lift smaller than ~50 pp is detectable.

**What a valid v2 requires.** Org-idiosyncratic decisions a model can't know a priori, with a no-memory pilot confirming control actually fails (~30–60%); temptation-neutral task phrasing; EXECUTABLE compliance checks (does the code use `==` on floats? does the Dockerfile contain `:latest`?) instead of an LLM judge, or ≥2 validated judges from different model families with rationales + Cohen's κ; a placebo arm (follow a distractor decision) to separate instruction-following from content; logged /recall queries + returned IDs scored for semantic-equivalence; and N≈50–100 for Wilson intervals narrower than ±20 pp.
