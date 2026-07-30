# Evaluation results

- corpus: **1643** decisions
- answerable queries: **60** (LLM-paraphrased, target = the source decision)
- unanswerable queries: **20** (off-topic; correct answer = return nothing)

| system | Recall@1 | Recall@3 | MRR@10 | abstained-on-answerable | abstention-acc (unanswerable) |
|---|---|---|---|---|---|
| dense (org-memory, w/ floor) | 65% | 82% | 0.736 | 0% | 100% |
| BM25 (lexical, no floor) | 68% | 77% | 0.731 | — | 25% |

**Reading it.** Recall@k / MRR measure whether the right decision is surfaced for an answerable query. *abstention-acc* is the fraction of OFF-TOPIC queries correctly answered with nothing — the differentiator: a lexical/dense system with no calibrated floor always returns its top-k, so it scores ~0% here while injecting irrelevant context. *abstained-on-answerable* is the cost side: answerable queries the floor wrongly rejected.

**Not run (external services):** mem0 (Chhikara et al. 2025), Zep (Rasmussen et al. 2025) — require standing deployments; wired as future baselines. The north-star withholding-A/B ('saves') needs an agent-in-the-loop and is specified in PAPER.md §7.

_Caveat: the answerable set is LLM-generated paraphrase, a proxy for real queries, not human-gold labels._
