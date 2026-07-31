#!/usr/bin/env python3
"""
v2c scorer — does the EXECUTABLE (deterministic-feedback) retry improve answers SEMANTICALLY,
per an independent cross-family judge (not the token check that drove the retry)?

Compares round1 (single-pass, top-1 injection, no retry) vs exec_final (after one deterministic
check-and-retry round). Judged blind (A/B shuffled) by a cross-family (fable) judge, so the loop
is not graded by the same check it optimizes, nor by the model family that wrote the answers.

Usage: python3 v2c_score.py [judge_out_file]   (default: v2c_judge_out_fable.jsonl)
"""
import json, math, os, sys
HERE = os.path.dirname(os.path.abspath(__file__))

def load(p):
    fp = os.path.join(HERE, p)
    return {json.loads(l)["id"]: json.loads(l) for l in open(fp) if l.strip()} if os.path.exists(fp) else {}

def wilson(k, n, z=1.96):
    if n == 0: return (0.0, 0.0)
    p=k/n; d=1+z*z/n; c=(p+z*z/(2*n))/d
    h=z*math.sqrt(p*(1-p)/n+z*z/(4*n*n))/d
    return (max(0,c-h), min(1,c+h))
def ci(k,n): lo,hi=wilson(k,n); return f"[{lo:.0%}, {hi:.0%}]"

def necessary_ids():
    # control-failed set from the earlier clean v2 partition (control judged in v2_judge_out.jsonl)
    out = load("v2_judge_out.jsonl"); mp = os.path.join(HERE, "v2_judge_map.json")
    if not out or not os.path.exists(mp): return None
    m = json.load(open(mp)); nec = set()
    for sid, verd in out.items():
        cl = [k for k, v in m[sid].items() if v == "control"]
        if cl and verd.get(cl[0], {}).get("complies") is False:
            nec.add(sid)
    return nec

def compliance(out, mp, arm, ids):
    return sum(1 for sid in ids if out.get(sid, {}).get([k for k, v in mp[sid].items() if v == arm][0], {}).get("complies"))

def main():
    jf = sys.argv[1] if len(sys.argv) > 1 else "v2c_judge_out_fable.jsonl"
    out = load(jf); mp = json.load(open(os.path.join(HERE, "v2c_judge_map.json")))
    ids = [s for s in mp if s in out]
    nec = necessary_ids()
    subsets = [("all scenarios", ids)]
    if nec: subsets.append((f"necessary subset (junior failed w/o memory)", [s for s in ids if s in nec]))

    L = [f"# v2c — executable check-and-retry, judged by an independent cross-family judge",
         f"", f"- judge output: `{jf}`  |  arms: round1 (no retry) vs exec_final (after deterministic retry)",
         f"- mechanical check (drove the retry, circular — for reference only): 16/26 → 22/26", ""]
    for name, sids in subsets:
        n = len(sids)
        r1 = compliance(out, mp, "round1", sids)
        fn = compliance(out, mp, "exec_final", sids)
        L += [f"## {name} (N={n})",
              f"| arm | semantic compliance | Wilson 95% |", f"|---|---|---|",
              f"| round1 (no retry) | {r1}/{n} = {r1/n:.0%} | {ci(r1,n)} |",
              f"| **exec_final (deterministic retry)** | **{fn}/{n} = {fn/n:.0%}** | {ci(fn,n)} |",
              f"", f"**Δ = {(fn-r1)/n:+.0%}** — "
              + ("the executable check-and-retry improved SEMANTIC compliance (independent judge), so the "
                 "deterministic loop produced genuinely-more-compliant answers, not token-stuffing."
                 if fn > r1 else
                 "no semantic improvement — the mechanical gain did not survive an independent semantic judge "
                 "(the retry may have added the right words without correct application)."),
              ""]
    L += ["_Caveat: N small; single cross-family judge; a token-heuristic drove the retry (not code execution). "
          "The point is methodological: the retry is now DRIVEN BY CODE with specific feedback, and VALIDATED BY "
          "AN INDEPENDENT judge of a different model family — not the model grading itself._"]
    p = os.path.join(HERE, "V2C_RESULTS.md")
    open(p, "w").write("\n".join(L) + "\n")
    print("\n".join(L)); print(f"\nwrote {p}")

if __name__ == "__main__":
    main()
