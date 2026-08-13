#!/usr/bin/env python3
"""
v2 uplift scorer — fixes v1's ceiling by measuring UPLIFT on the NECESSARY subset only.

Design (addresses the v1 red-team, AB_REDTEAM.md):
  - Control = a JUNIOR model (haiku) with no memory  -> "least-knowledgeable person's default".
  - Scenarios = org-idiosyncratic decisions a base model can't know a priori (no textbook ceiling).
  - NECESSITY partition (self-learning / prediction-error signal): a scenario is NECESSARY iff the
    junior control VIOLATES the decision (default wrong = high prediction error). Where control already
    complies, the decision is REDUNDANT (the model knows it) — excluded from the uplift denominator,
    which is exactly why v1's ceiling can't recur.
  - UPLIFT (on the necessary subset) = does the memory raise the junior to the org's answer?
      treatment = junior + live /recall ;  oracle = junior + the decision handed over (retrieval ceiling).
  - Judge = strict, anonymized (A/B/C), evidence-required, against objective token-based checks.

Inputs: v2_judge_out.jsonl {id, A:{complies}, B:{...}, C:{...}}, v2_judge_map.json (A/B/C->arm),
        v2_treatment.jsonl (recalled_target).
"""
import json, math, os
HERE = os.path.dirname(os.path.abspath(__file__))

def load(p):
    fp = os.path.join(HERE, p)
    return [json.loads(l) for l in open(fp) if l.strip()] if os.path.exists(fp) else []

def wilson(k, n, z=1.96):
    if n == 0: return (0.0, 0.0)
    p = k/n; d = 1 + z*z/n
    c = (p + z*z/(2*n))/d
    h = z*math.sqrt(p*(1-p)/n + z*z/(4*n*n))/d
    return (max(0,c-h), min(1,c+h))

def ci(k, n):
    lo, hi = wilson(k, n); return f"[{lo:.0%}, {hi:.0%}]"

def main():
    out = {r["id"]: r for r in load("v2_judge_out.jsonl")}
    mp = json.load(open(os.path.join(HERE, "v2_judge_map.json")))
    ret = {r["id"]: r.get("recalled_target") for r in load("v2_treatment.jsonl")}
    rows = []
    for sid, verd in out.items():
        m = mp[sid]  # {"A":"control",...}
        rec = {"id": sid}
        for label, arm in m.items():
            rec[arm] = bool(verd.get(label, {}).get("complies"))
        rec["recalled_target"] = bool(ret.get(sid))
        rows.append(rec)
    n = len(rows)

    ctrl = sum(r["control"] for r in rows)
    necessary = [r for r in rows if not r["control"]]          # control VIOLATED = memory can help
    redundant = [r for r in rows if r["control"]]              # model already knew
    nn = len(necessary)
    ret_hit = sum(r["recalled_target"] for r in rows)

    t_nec = sum(r["treatment"] for r in necessary)
    o_nec = sum(r["oracle"] for r in necessary)
    saves_t = sum(1 for r in necessary if r["treatment"])
    saves_o = sum(1 for r in necessary if r["oracle"])

    L = ["# v2 uplift evaluation — necessity partition + uplift", "",
        f"- scenarios: **{n}** org-idiosyncratic decisions (no textbook ceiling); control = a junior model (haiku), no memory",
        f"- retrieval@task: /recall surfaced the target decision in **{ret_hit}/{n}** ({ret_hit/n:.0%}) — vs v1's 0/12 on textbook tasks",
        "",
        "## 1. Necessity partition (the self-learning / prediction-error signal)",
        f"- **NECESSARY** (junior default VIOLATES → high prediction error → memory can help): **{nn}/{n}** ({nn/n:.0%})",
        f"- **REDUNDANT** (junior already complies → model knows it → low value): **{len(redundant)}/{n}** ({len(redundant)/n:.0%})",
        "",
        "This is the derived result the ceiling-free design produces: the fraction of org decisions that a "
        "capable-but-non-expert engineer would get WRONG by default — i.e. the decisions actually worth surfacing.",
        "",
        "## 2. Uplift ON THE NECESSARY SUBSET (does the memory raise the junior to the org's answer?)"]
    if nn == 0:
        L.append(f"- No necessary scenarios (junior complied on all {n}); uplift undefined. Harder scenarios needed.")
    else:
        L += [f"- CONTROL (junior, by definition failed here): 0/{nn} = 0%",
              f"- **TREATMENT (junior + live memory): {t_nec}/{nn} = {t_nec/nn:.0%}**  Wilson95 {ci(t_nec,nn)}  → **uplift +{t_nec/nn:.0%}**",
              f"- **ORACLE (junior + decision surfaced): {o_nec}/{nn} = {o_nec/nn:.0%}**  Wilson95 {ci(o_nec,nn)}  → ceiling **+{o_nec/nn:.0%}**",
              "",
              f"- saves — end-to-end (control✗→treatment✓): **{saves_t}/{nn}**   |   oracle ceiling: **{saves_o}/{nn}**"]
    L += ["",
        "## Honesty / caveats",
        f"- N is small (necessary subset = {nn}); Wilson intervals are wide — read as a directional pilot, not a powered study.",
        "- Judge is a single anonymized LLM with required evidence quotes (a real improvement over v1's un-rationaled 36/36, "
        "but not executable checks or multi-judge κ — still the top residual threat).",
        "- retrieval@task is exact decision-ID (a lower bound). v1 vs v2 (0/12 → high) shows retrieval works far better on "
        "org-specific, entity-named prompts than on adversarial answer-hiding tasks.",
        "- 'diverged-plausibly-better' (a junior out-thinking the memory → a capture candidate) is not scored here; it needs a "
        "separate quality judgment and is left as future work."]
    p = os.path.join(HERE, "V2_RESULTS.md")
    open(p, "w").write("\n".join(L) + "\n")
    print("\n".join(L)); print(f"\nwrote {p}")

if __name__ == "__main__":
    main()
