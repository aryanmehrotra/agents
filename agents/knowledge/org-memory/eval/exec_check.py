#!/usr/bin/env python3
"""
Executable (deterministic) compliance checker for the check-and-retry loop.

Each scenario's `check` field names COMPLIANCE tokens and VIOLATION tokens as quoted strings, e.g.:
  "Compliant iff ... Compliance tokens: 'aggregator', 'gRPC', 'GetMetrics'. Violation tokens: 'FINAL', ..."

This module extracts those quoted tokens and evaluates an answer MECHANICALLY (case-insensitive
substring; a token containing '/' is a set of alternatives, any of which satisfies it):
  PASS  iff  (>=1 compliance token present)  AND  (no violation token present)
On FAIL it returns the specific missing compliance tokens and the specific present violation tokens —
that is the deterministic feedback the retry loop hands back to the model. This is NOT an LLM judging
itself; it is code. (It is heuristic — quoted-token matching, not code execution — and is used to DRIVE
retries; final grading is still done by the independent semantic judge, so the loop can't grade itself.)

CLI:  python3 exec_check.py <answers.jsonl>   -> prints pass/fail summary + writes <answers>.check.jsonl
      (answers.jsonl lines: {"id","answer"})
"""
import json, re, sys, os

HERE = os.path.dirname(os.path.abspath(__file__))

def quoted(s):
    return [t.strip() for t in re.findall(r"'([^']+)'", s or "")]

def parse_check(check):
    # split compliance side vs violation side on the word "Violation"
    parts = re.split(r"[Vv]iolation", check, maxsplit=1)
    comp = quoted(parts[0])
    viol = quoted(parts[1]) if len(parts) > 1 else []
    return comp, viol

def token_present(tok, text):
    # 'a/b/c' => any alternative present
    return any(alt.strip().lower() in text for alt in tok.split("/") if alt.strip())

_NEG = re.compile(r"^(no|not|never|without)\s+", re.I)

def evaluate(answer, check):
    text = (answer or "").lower()
    comp, viol = parse_check(check)
    # A "no X"/"never X" compliance token is an ABSENCE requirement (X must NOT appear), not a substring.
    pos = [t for t in comp if not _NEG.match(t)]
    absence = [(_NEG.sub("", t).strip(), t) for t in comp if _NEG.match(t)]  # (referent, original)

    missing = [t for t in pos if not token_present(t, text)]                 # positive tokens absent
    present_viol = [t for t in viol if token_present(t, text)]               # explicit violation tokens present
    broke_absence = [orig for ref, orig in absence if token_present(ref, text)]  # "no X" but X present

    has_pos = len(pos) == 0 or len(missing) < len(pos)  # at least one positive token present
    ok = has_pos and not present_viol and not broke_absence
    return ok, missing, present_viol + broke_absence

def feedback(missing, present_viol):
    f = []
    if missing:
        f.append("Your answer does not mention the required mechanism(s): " +
                 "; ".join(missing[:4]) + ". Use the exact mechanism, not a generic paraphrase.")
    if present_viol:
        f.append("Your answer does the FORBIDDEN thing: " + "; ".join(present_viol[:4]) +
                 ". Remove it and use the team's approach instead.")
    return " ".join(f)

def main():
    ans = {json.loads(l)["id"]: json.loads(l) for l in open(sys.argv[1]) if l.strip()}
    scen = {json.loads(l)["id"]: json.loads(l) for l in open(os.path.join(HERE, "v2_scenarios.jsonl")) if l.strip()}
    out = sys.argv[1].replace(".jsonl", "") + ".check.jsonl"
    w = open(out, "w"); npass = 0; scored = 0
    for sid, a in ans.items():
        if sid not in scen:  # skip a malformed/foreign id
            continue
        scored += 1
        ok, missing, viol = evaluate(a.get("answer", ""), scen[sid]["check"])
        npass += ok
        w.write(json.dumps({"id": sid, "pass": ok, "missing": missing,
                            "violations": viol, "feedback": feedback(missing, viol)}) + "\n")
    w.close()
    print(f"executable check: {npass}/{scored} PASS  ->  {out}")

if __name__ == "__main__":
    main()
