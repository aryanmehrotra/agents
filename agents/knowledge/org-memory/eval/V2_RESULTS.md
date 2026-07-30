# v2 uplift evaluation — necessity partition + uplift

- scenarios: **26** org-idiosyncratic decisions (no textbook ceiling); control = a junior model (haiku), no memory
- retrieval@task: /recall surfaced the target decision in **25/26** (96%) — vs v1's 0/12 on textbook tasks

## 1. Necessity partition (the self-learning / prediction-error signal)
- **NECESSARY** (junior default VIOLATES → high prediction error → memory can help): **17/26** (65%)
- **REDUNDANT** (junior already complies → model knows it → low value): **9/26** (35%)

This is the derived result the ceiling-free design produces: the fraction of org decisions that a capable-but-non-expert engineer would get WRONG by default — i.e. the decisions actually worth surfacing.

## 2. Uplift ON THE NECESSARY SUBSET (does the memory raise the junior to the org's answer?)
- CONTROL (junior, by definition failed here): 0/17 = 0%
- **TREATMENT (junior + live memory): 0/17 = 0%**  Wilson95 [0%, 18%]  → **uplift +0%**
- **ORACLE (junior + decision surfaced): 17/17 = 100%**  Wilson95 [82%, 100%]  → ceiling **+100%**

- saves — end-to-end (control✗→treatment✓): **0/17**   |   oracle ceiling: **17/17**

## Honesty / caveats
- N is small (necessary subset = 17); Wilson intervals are wide — read as a directional pilot, not a powered study.
- Judge is a single anonymized LLM with required evidence quotes (a real improvement over v1's un-rationaled 36/36, but not executable checks or multi-judge κ — still the top residual threat).
- retrieval@task is exact decision-ID (a lower bound). v1 vs v2 (0/12 → high) shows retrieval works far better on org-specific, entity-named prompts than on adversarial answer-hiding tasks.
- 'diverged-plausibly-better' (a junior out-thinking the memory → a capture candidate) is not scored here; it needs a separate quality judgment and is left as future work.

## Diagnosis — the value leaks in the LAST MILE (surfacing ≠ compliance)

The oracle−treatment gap (100% vs 0% on the necessary subset) is not retrieval:
- retrieval@task on the necessary subset = **16/17 (94%)** — the memory *did* surface the right decision.
- so treatment failed on **16/17 surfaced-but-under-applied** cases, and only **1/17** was a true retrieval miss.

The mechanism: a JUNIOR (weak) agent, handed a **top-3 advisory recall list**, paraphrases and drops the
org-specific detail (answers "SELECT … FOR UPDATE" but omits "SKIP LOCKED"; "batch read" instead of the
aggregator gRPC path), which a strict check fails. The **same decision as one authoritative instruction
(oracle) is followed fully (100%)**. So: the memory's CONTENT is validated (oracle ceiling +100%),
retrieval largely works (16/17), but the **recall→inject→weak-agent last mile realizes ~none of it**.

Design implications (each testable next): (1) inject the single highest-confidence decision as an
**authoritative directive**, not a top-3 "advisory context" list; (2) the bottleneck is application, not
retrieval — matches the brain "central-executive" gap in BRAIN.md (context binds the decision but nothing
forces the agent to act on it); (3) a stronger agent, or a compliance-check-and-retry loop, likely closes
much of the gap. This is the honest, non-obvious result of v2: **for the least-knowledgeable user, getting
the memory to the agent is easy; getting the agent to actually FOLLOW it is the hard part.**
