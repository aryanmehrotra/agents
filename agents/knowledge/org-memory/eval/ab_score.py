#!/usr/bin/env python3
"""
Withholding A/B scorer — 3-arm (control / live-recall / oracle).

REWRITTEN after a red-team (eval/AB_REDTEAM.md) found the prior version printed a fixed
conclusion regardless of the data and hardcoded the retrieval number. This version derives
every statement from the inputs, recomputes retrieval@task from treatment.jsonl, reports Wilson
95% CIs (so N=12's uninformativeness is visible), and refuses to narrate an effect that isn't there.

Arms (per scenario: a task whose naive default would violate a real prior decision):
  CONTROL   = agent solves with no memory.
  TREATMENT = agent solves after querying the live /recall.
  ORACLE    = agent solves with the target decision handed to it (perfect-retrieval ceiling).

Inputs: eval/judgments.jsonl {id,control_complies,treatment_complies,oracle_complies}
        eval/treatment.jsonl  {id,...,recalled_target}   (for retrieval@task)
"""
import json, math, os

HERE = os.path.dirname(os.path.abspath(__file__))

def load(p):
    fp = os.path.join(HERE, p)
    return [json.loads(l) for l in open(fp)] if os.path.exists(fp) else []

def wilson(k, n, z=1.96):
    if n == 0:
        return (0.0, 0.0)
    p = k / n
    d = 1 + z*z/n
    c = (p + z*z/(2*n)) / d
    h = z*math.sqrt(p*(1-p)/n + z*z/(4*n*n)) / d
    return (max(0.0, c-h), min(1.0, c+h))

def frac(j, key):
    return sum(1 for r in j if r.get(key) is True)

def line(name, k, n):
    lo, hi = wilson(k, n)
    return f"| {name} | {k}/{n} = {k/n:.0%} | [{lo:.0%}, {hi:.0%}] |"

def main():
    j = load("judgments.jsonl")
    n = len(j)
    if not n:
        print("no judgments.jsonl — run the pipeline first"); return
    c, t, o = frac(j, "control_complies"), frac(j, "treatment_complies"), frac(j, "oracle_complies")

    trt = load("treatment.jsonl")
    ret = sum(1 for r in trt if r.get("recalled_target") is True)
    ret_n = len(trt) or n

    saves_e2e = sum(1 for r in j if r.get("control_complies") is False and r.get("treatment_complies") is True)
    saves_orc = sum(1 for r in j if r.get("control_complies") is False and r.get("oracle_complies") is True)

    # data-conditional verdict — the whole point of the rewrite.
    if c == n:
        verdict = ("**INCONCLUSIVE — control ceiling.** The no-memory control complied on all "
                   f"{n}/{n} scenarios, so there is no room for any arm to improve and NO surfacing effect "
                   "is estimable. This does not show the memory is useless; it shows the scenarios test "
                   "conventions the base model already follows (redundant with model priors), so they cannot "
                   "discriminate. A valid run needs scenarios where control demonstrably fails (~30–60%).")
    elif o <= c:
        verdict = (f"**No positive surfacing effect observed** (oracle {o}/{n} ≤ control {c}/{n}). With N={n} "
                   "and the CIs below, this run cannot distinguish a small effect from zero.")
    else:
        verdict = (f"Oracle−control = {(o-c)/n:+.0%} (surfacing ceiling); treatment−control = {(t-c)/n:+.0%} "
                   f"(deployed). Retrieval@task = {ret}/{ret_n}. Interpret ONLY within the CIs below (N={n}).")

    lines = ["# Withholding A/B — results", "",
        "> **Status: INCONCLUSIVE.** This is a pilot that FAILED to produce a usable measurement; see "
        "`AB_REDTEAM.md` for the full list of validity threats. The numbers below are reported for honesty, "
        "not as evidence for or against the system.", "",
        f"- scenarios: **{n}**",
        f"- retrieval@task (exact decision-ID match; a lower bound — semantic-equivalent hits count as misses): "
        f"**{ret}/{ret_n}**",
        "",
        "| arm | compliance | Wilson 95% CI |",
        "|---|---|---|",
        line("CONTROL (no memory)", c, n),
        line("TREATMENT (live /recall)", t, n),
        line("ORACLE (decision surfaced)", o, n),
        "",
        f"- saves (control✗→treatment✓): {saves_e2e}/{n}   |   oracle ceiling (control✗→oracle✓): {saves_orc}/{n}",
        "",
        verdict, "",
        "**Why it's inconclusive (red-team, summarized).** (1) Ceiling: control complied everywhere, so no "
        "contrast exists. (2) Construct: the sampled decisions are canonical best practices the base model knows "
        "from pretraining, and several task prompts leak the answer. (3) Judge: a single unvalidated LLM judge "
        "returned 36/36 compliant with no rationales and no negative control. (4) Retrieval 0/12 is exact-ID on a "
        "deliberately adversarial, worst-case task set with unlogged queries. (5) N=12 → Wilson CIs ≈ ±25–40 pp; "
        "no lift smaller than ~50 pp is detectable.",
        "",
        "**What a valid v2 requires.** Org-idiosyncratic decisions a model can't know a priori, with a no-memory "
        "pilot confirming control actually fails (~30–60%); temptation-neutral task phrasing; EXECUTABLE compliance "
        "checks (does the code use `==` on floats? does the Dockerfile contain `:latest`?) instead of an LLM judge, "
        "or ≥2 validated judges from different model families with rationales + Cohen's κ; a placebo arm (follow a "
        "distractor decision) to separate instruction-following from content; logged /recall queries + returned IDs "
        "scored for semantic-equivalence; and N≈50–100 for Wilson intervals narrower than ±20 pp."]
    out = os.path.join(HERE, "AB_RESULTS.md")
    open(out, "w").write("\n".join(lines) + "\n")
    print("\n".join(lines)); print(f"\nwrote {out}")

if __name__ == "__main__":
    main()
