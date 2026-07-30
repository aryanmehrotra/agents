#!/usr/bin/env python3
"""
v2b scorer — does authoritative top-1 injection + self-check-retry close the last-mile gap?

Four arms judged together (anonymized A/B/C/D):
  control   : junior, no memory
  treatment : junior + top-3 advisory recall list   (the v2 result: 0% on necessary)
  directive : junior + top-1 as a MANDATORY convention + self-verify-and-retry   (the fix)
  oracle    : junior + the decision handed over       (content ceiling: 100%)

Uplift is measured on the NECESSARY subset (control failed). The question: does `directive`
move from treatment's 0% toward oracle's 100%?
"""
import json, math, os
HERE = os.path.dirname(os.path.abspath(__file__))

def load(p):
    fp = os.path.join(HERE, p)
    return [json.loads(l) for l in open(fp) if l.strip()] if os.path.exists(fp) else []

def wilson(k, n, z=1.96):
    if n == 0: return (0.0, 0.0)
    p=k/n; d=1+z*z/n; c=(p+z*z/(2*n))/d
    h=z*math.sqrt(p*(1-p)/n+z*z/(4*n*n))/d
    return (max(0,c-h), min(1,c+h))
def ci(k,n): lo,hi=wilson(k,n); return f"[{lo:.0%}, {hi:.0%}]"

def main():
    out={r["id"]: r for r in load("v2b_judge_out.jsonl")}
    mp=json.load(open(os.path.join(HERE,"v2b_judge_map.json")))
    arms=["control","treatment","directive","oracle"]
    rows=[]
    for sid,verd in out.items():
        m=mp[sid]; rec={"id":sid}
        for label,arm in m.items():
            rec[arm]=bool(verd.get(label,{}).get("complies"))
        rows.append(rec)
    n=len(rows)
    nec=[r for r in rows if not r["control"]]
    nn=len(nec)
    res={a: sum(r[a] for r in nec) for a in arms}

    L=["# v2b — does authoritative top-1 injection + retry close the last-mile gap?", "",
       f"- scenarios judged: **{n}**  |  necessary subset (junior failed w/o memory): **{nn}**", "",
       "## Uplift on the necessary subset", "",
       "| arm | compliance | Wilson 95% |", "|---|---|---|",
       f"| control (no memory) | 0/{nn} = 0% | — |",
       f"| treatment (top-3 advisory list) | {res['treatment']}/{nn} = {res['treatment']/nn:.0%} | {ci(res['treatment'],nn)} |",
       f"| **directive (top-1 authoritative + retry)** | **{res['directive']}/{nn} = {res['directive']/nn:.0%}** | {ci(res['directive'],nn)} |",
       f"| oracle (decision handed over) | {res['oracle']}/{nn} = {res['oracle']/nn:.0%} | {ci(res['oracle'],nn)} |",
       "",
       f"**Gap closed:** treatment {res['treatment']/nn:.0%} → directive {res['directive']/nn:.0%} "
       f"(oracle ceiling {res['oracle']/nn:.0%}). "
       + ("The authoritative-directive + retry injection materially closes the surfacing→compliance gap."
          if res['directive']>res['treatment'] else
          "Authoritative injection did NOT close the gap — the bottleneck is the weak agent's application, not the format."),
       "",
       "_Caveat: N small, single anonymized LLM judge (evidence-required), directional pilot. The directive "
       "arm's self-check-retry is model self-report; an executable check-and-retry loop is the stronger form._"]
    p=os.path.join(HERE,"V2B_RESULTS.md")
    open(p,"w").write("\n".join(L)+"\n")
    print("\n".join(L)); print(f"\nwrote {p}")

if __name__=="__main__":
    main()
