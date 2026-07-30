# v2b — does authoritative top-1 injection + retry close the last-mile gap?

- scenarios judged: **25**  |  necessary subset (junior failed w/o memory): **13**

## Uplift on the necessary subset

| arm | compliance | Wilson 95% |
|---|---|---|
| control (no memory) | 0/13 = 0% | — |
| treatment (top-3 advisory list) | 0/13 = 0% | [0%, 23%] |
| **directive (top-1 authoritative + retry)** | **11/13 = 85%** | [58%, 96%] |
| oracle (decision handed over) | 13/13 = 100% | [77%, 100%] |

**Gap closed:** treatment 0% → directive 85% (oracle ceiling 100%). The authoritative-directive + retry injection materially closes the surfacing→compliance gap.

_Caveat: N small, single anonymized LLM judge (evidence-required), directional pilot. The directive arm's self-check-retry is model self-report; an executable check-and-retry loop is the stronger form._
