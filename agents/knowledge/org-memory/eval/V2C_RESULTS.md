# v2c — executable check-and-retry, judged by an independent cross-family judge

- judge output: `v2c_judge_out_fable.jsonl`  |  arms: round1 (no retry) vs exec_final (after deterministic retry)
- mechanical check (drove the retry, circular — for reference only): 16/26 → 22/26

## all scenarios (N=26)
| arm | semantic compliance | Wilson 95% |
|---|---|---|
| round1 (no retry) | 23/26 = 88% | [71%, 96%] |
| **exec_final (deterministic retry)** | **23/26 = 88%** | [71%, 96%] |

**Δ = +0%** — no semantic improvement — the mechanical gain did not survive an independent semantic judge (the retry may have added the right words without correct application).

## necessary subset (junior failed w/o memory) (N=17)
| arm | semantic compliance | Wilson 95% |
|---|---|---|
| round1 (no retry) | 14/17 = 82% | [59%, 94%] |
| **exec_final (deterministic retry)** | **14/17 = 82%** | [59%, 94%] |

**Δ = +0%** — no semantic improvement — the mechanical gain did not survive an independent semantic judge (the retry may have added the right words without correct application).

_Caveat: N small; single cross-family judge; a token-heuristic drove the retry (not code execution). The point is methodological: the retry is now DRIVEN BY CODE with specific feedback, and VALIDATED BY AN INDEPENDENT judge of a different model family — not the model grading itself._

## Verdict (honest)

- **Inter-judge agreement: 52/52 (100%)** between the fable (cross-family) and the default judge — the judging is reliable.
- **The executable retry did not improve semantic compliance** (88%→88% all; 82%→82% necessary), despite lifting the token check 16→22. Two reasons, both important:
  1. **The injection is the lever, not the retry.** Single-pass authoritative top-1 injection with NO retry already scored 82–88% — the same as v2b's directive+retry (85%). The value came from presenting the top decision authoritatively; the retry step adds ~nothing.
  2. **The token check is a gameable proxy (Goodhart).** Driving retries by "does the answer contain token X" made answers *pass the token check* without becoming *more correct* — they largely already applied the mechanism; the check just had false negatives on phrasing. Optimizing the proxy did not move the goal.
- **Implication:** a genuinely rigorous executable check requires **code execution** (compile/run/test the generated code), not quoted-token matching. For design-level decisions that resist execution, an independent multi-judge panel (validated, cross-family — we saw 100% agreement) is the better instrument.
