# Research foundations

org-memory is not a pile of heuristics. Every non-obvious design decision traces to an
established result in statistics, causal inference, or the retrieval/agent-memory literature.
This document maps **decision → paper → what it justifies**, and — just as importantly — marks
where we are *extrapolating* past what a paper actually proves.

The through-line: **capturing the *why* behind decisions is easy; learning whether a decision was
*correct* from what happened afterward is provably hard.** org-memory is built around that
asymmetry. It retrieves and advises; it does not pretend to grade itself from downstream outcomes.

---

## 1. The honest floor — why we do *not* learn per-rule correctness from outcomes

**Design decision.** org-memory never adjusts a decision's "correctness" score from downstream
engineering outcomes (did the PR revert? did the incident recur?). Feedback is an *explicit human
signal* (`helpful` / `wrong` / `outdated`), not an inferred causal effect. This looks like a missing
feature. It is a deliberate refusal, and it is the single most important design choice in the system.

**Why it's forced — four results that compose:**

- **Confounding — Pearl, *Causality: Models, Reasoning, and Inference* (2009, Cambridge UP).**
  Applying a rule is a *treatment that is not randomly assigned*. Engineers apply a rule precisely
  in the contexts where they judge it relevant, so the decision to apply is entangled with the
  context. The back-door criterion tells you which confounders you'd have to adjust for to recover a
  causal effect — and we can observe none of them. Ch. 6 (Simpson's paradox) shows an unadjusted
  outcome correlation can *reverse sign* under confounding: a rule that genuinely helps can look
  harmful in the raw data, and vice-versa.

- **The counterfactual is never observed — Heckman, "Sample Selection Bias as a Specification
  Error" (*Econometrica*, 1979).** We only see outcomes for the rule–context pairs that actually
  occurred. That is a *selected*, non-random sample; OLS/naive averaging on it is biased because
  selection acts as an omitted variable. The outcome "had we applied the rule in the context where
  we didn't" is structurally missing.

- **Recovering a causal effect needs exogenous variation — Imbens & Angrist, "Identification and
  Estimation of Local Average Treatment Effects" (*Econometrica*, 1994); Angrist & Pischke,
  *Mostly Harmless Econometrics* (2009).** To identify a rule's effect you'd need an *instrument*:
  something that perturbs whether the rule is applied but is otherwise unrelated to the outcome.
  Pure observation of an engineering org supplies no such instrument.

- **Even with infinite data it can be non-identifiable — Cramér–Rao (Rao 1945; Cramér 1946).**
  For any unbiased estimator, `var(θ̂) ≥ 1/I(θ)` — variance is floored by the inverse Fisher
  information. If nothing independently perturbs the apply-decision, the Fisher information for the
  rule's isolated effect is *singular*, the bound diverges, and **no estimator at any sample size can
  pin the parameter down.** More data does not save you.

**Honest-scope note.** The singular-information ⇒ non-identifiability step is the standard textbook
corollary of Cramér–Rao, not a line quoted from the CRB paper itself. The composition of these four
results into "you cannot learn per-rule correctness from outcomes" is *our* argument built from
published pieces — each piece is real; the synthesis is the design's thesis, not a cited theorem.

---

## 2. The escape — predict from features, then *abstain*

If we can't learn correctness from outcomes, what *can* we do? Predict relevance from *features* of
the current context, and — the crucial part — **decline to answer when we're not confident.** An
empty recall is a first-class, correct output, not a failure.

- **Chow, "On Optimum Recognition Error and Reject Tradeoff" (*IEEE Trans. Inf. Theory*, 1970).**
  The optimal reject rule: abstain when the top posterior probability is below a cost-derived
  threshold. This is the ancestor of our `retrieve.precision_floor`.

- **El-Yaniv & Wiener, "On the Foundations of Noise-Free Selective Classification" (*JMLR*, 2010);
  Cortes, DeSalvo & Mohri, "Learning with Rejection" (*ALT*, 2016).** Formalize the
  **risk–coverage tradeoff**: you can guarantee a target error rate by giving up coverage
  (answering less often). org-memory trades coverage for precision *on purpose* — it would rather
  stay silent than inject a marginally-relevant decision.

- **Geifman & El-Yaniv, "Selective Classification for Deep Neural Networks" (NeurIPS 2017);
  "SelectiveNet" (ICML 2019).** Modern selective prediction: a model configured to hit a desired
  error rate at maximal coverage. Our floor is the simple, inspectable version of the same idea.

**This is why the precision floor exists and why "inject nothing" is instrumented as a success
metric, not an error** (see `ValueStats.RecallsEmpty`, surfaced in the Value tab).

---

## 3. Calibrated abstention — the principled upgrade path

The precision floor is a hand-tuned threshold (currently `0.60`, tuned for a tight single-repo
corpus). The literature gives a way to make that threshold carry a *formal guarantee* instead of a
vibe — the natural Phase-2 upgrade.

- **Vovk, Gammerman & Shafer, *Algorithmic Learning in a Random World* (Springer, 2005; 2nd ed.
  2022); origins in Gammerman, Vovk & Vapnik, "Learning by Transduction" (UAI 1998).** Conformal
  prediction: distribution-free prediction sets with **finite-sample validity** under
  exchangeability. A conformal calibration set would let us set the floor to guarantee, e.g., "≤10%
  of injected decisions are irrelevant" rather than eyeballing it.

- **Angelopoulos & Bates, "A Gentle Introduction to Conformal Prediction" (arXiv:2107.07511, 2021);
  Angelopoulos et al., "Conformal Risk Control" (arXiv:2208.02814, 2022).** The practitioner path
  and the extension from miscoverage to *any monotone loss* — directly applicable to bounding an
  irrelevant-injection rate.

- **Abbasi Yadkori, Kuzborskij, Stutz, György, Fisch, et al., "Mitigating LLM Hallucinations via
  Conformal Abstention" (arXiv:2405.01563, 2024).** The closest prior art to what we're doing:
  conformal prediction driving an LLM *abstain* decision with a rigorous bound on the bad-output
  rate. This is the paper to implement against when the floor graduates from heuristic to guarantee.

---

## 4. Why naive outcome-based evaluation is biased — the covariate-shift argument

A second, independent reason the "learn from outcomes" temptation fails — even setting aside
confounding: the contexts org-memory is *used* in drift away from the contexts it was *tuned* on.

- **Shimodaira, "Improving Predictive Inference Under Covariate Shift…" (*J. Stat. Plan. Inf.*,
  2000).** Under `P_train(x) ≠ P_test(x)`, ordinary maximum likelihood and model selection are
  biased; you need density-ratio importance weighting to correct.

- **Sugiyama, Krauledat & Müller, "Covariate Shift Adaptation by Importance Weighted Cross
  Validation" (*JMLR*, 2007).** Shows ordinary cross-validation — i.e. naive outcome-based
  evaluation — is *biased* under covariate shift, and gives the unbiased weighted estimator.

**Implication for our metrics.** The Value tab reports *estimated* token savings and *explicit*
helpful/wrong tallies. It deliberately does **not** claim a causal "org-memory improved outcomes by
X%" — that number would be covariate-shift-biased and confounded (§1). A rigorous lift claim needs a
withholding A/B (randomized exogenous variation), which is scoped as future work, not faked in the
dashboard.

---

## 5. Retrieve-then-advise — the RAG backbone

- **Lewis et al., "Retrieval-Augmented Generation for Knowledge-Intensive NLP Tasks" (NeurIPS
  2020, arXiv:2005.11401); Guu et al., "REALM" (ICML 2020, arXiv:2002.08909).** The pattern:
  condition generation on retrieved non-parametric knowledge instead of baking it into weights.
  org-memory is a RAG *knowledge source specialized to decisions-and-their-why*, surfaced to a
  coding agent via the recall hook / MCP tool.

---

## 6. Asymmetric dense retrieval — why `search_query:` ≠ `search_document:`

- **Karpukhin et al., "Dense Passage Retrieval" (EMNLP 2020, arXiv:2004.04906).** The canonical
  dual-encoder: *separate* encoders for the query and the passage. Asymmetry is the point — a
  question and the passage answering it are not the same kind of text.

- **Wang et al., "E5" (arXiv:2212.03533, 2022); Izacard et al., "Contriever"
  (arXiv:2112.09118, 2021); Nussbaum et al., "Nomic Embed" (arXiv:2402.01613, 2024).** E5
  introduces the `query:` / `passage:` input-prefix scheme; Nomic Embed (the `nomic-embed-text`
  model we default to) uses `search_query:` / `search_document:` prefixes.

**Honest-scope note.** The `search_query:`/`search_document:` prefixes are documented in the Nomic
Embed **model card**, not the arXiv abstract. The asymmetric-prefix *idea* is confirmable from the
E5 paper; the exact Nomic prefix strings come from model docs. Our `embed_http.go` applies
`RoleDocument` at capture and `RoleQuery` at recall precisely to honor this asymmetry.

---

## 7. The memory model — recency, importance, relevance, hierarchy

- **Park et al., "Generative Agents: Interactive Simulacra of Human Behavior" (UIST 2023,
  arXiv:2304.03442).** The **memory stream**: store experiences in natural language, retrieve by a
  weighted combination of **recency + importance + relevance**, and synthesize higher-level
  *reflections*. Our `rank.go` scoring (`wRel*sim + wRec*recency + wImp*importance`) is a direct
  descendant; the scope hierarchy (child → parent, with recall inheriting along confirmed relations)
  is a structured analogue of reflection/consolidation.

- **Packer et al., "MemGPT: Towards LLMs as Operating Systems" (arXiv:2310.08560, 2023).** Tiered
  memory paged in and out of a bounded context window — the justification for retrieving a *small*
  relevant set on demand rather than dumping the whole store into the prompt.

---

## 8. Why we inject *few* items, or none — the cost side

- **Liu et al., "Lost in the Middle: How Language Models Use Long Contexts" (TACL 2024,
  arXiv:2307.03172).** Models use information best at the *start and end* of a context and degrade
  sharply when relevant content sits in the middle. Stuffing many decisions in is not just token-
  expensive — it actively *buries* the relevant one. This is the empirical backing for `top_k = 3`,
  the precision floor, and the whole "restraint is a feature" stance.

---

## 9. The human-memory model — context, consolidation, forgetting

The system stores knowledge like **git** (auto-detected context, groupable), but curates it like a
**brain** (keep the important, let the trash fade). Git never forgets — correct for code, wrong for a
memory. Three bodies of human-memory research shape the curation layer.

### 9a. Context is a retrieval PRIOR, not a wall (`engine.go` soft-context)
- **Tulving & Thomson, "Encoding specificity…" (*Psych. Review*, 1973).** A retrieval cue works only
  to the extent it matches the encoding context — so a decision made in one repo is most reliably
  surfaced when that repo's context recurs.
- **Godden & Baddeley, "Context-dependent memory… on land and underwater" (*Br. J. Psych.*, 1975).**
  Divers recalled more when the test environment matched the learning environment. (Honest-scope: a
  2021 replication, Murre, *R. Soc. Open Sci.*, found the effect real but weak, d≈0.25.)
- **Bower, "Mood and memory" (*American Psychologist*, 1981).** State/mood at retrieval acts as a cue
  — context is broader than location.
- **Gao et al., "CDMem" (NAACL 2025).** An LLM-agent framework that indexes memory by task/environment
  context — the direct engineering instance of "context as a retrieval key."

  **Design consequence:** repo/scope is a strong *prior* (specificity boost), **not** a hard filter —
  else generalizable wisdom gets walled inside the repo it was recorded in. This is exactly the
  soft-context change: relevant-cross-repo still surfaces; same-repo is preferred.

### 9b. The best is consolidated — importance gates persistence (roadmap: importance scoring)
- **McClelland, McNaughton & O'Reilly, "Complementary Learning Systems" (*Psych. Review*, 1995);
  Squire & Alvarez, standard consolidation theory (*Curr. Opin. Neurobiol.*, 1995).** Fast episodic
  store → slow structured store; not everything is retained equally.
- **Frey & Morris, "Synaptic tagging and LTP" (*Nature*, 1997).** Only *salient* events capture the
  proteins that make a trace last — importance gates consolidation at the synapse.
- **Diekelmann & Born, "The memory function of sleep" (*Nat. Rev. Neurosci.*, 2010);** **Castel,
  value-directed remembering (*Dev. Psych.*, 2011);** **McGaugh, "Memory — a century of
  consolidation" (*Science*, 2000).** Sleep replay, *value*, and *emotional arousal* all preferentially
  strengthen memories worth keeping.
- **ML analogues:** **Schaul et al., "Prioritized Experience Replay" (arXiv:1511.05952, 2015)** —
  replay by surprise/importance; **Park et al., Generative Agents (2023)** — an explicit importance
  gate on what's elevated; **Zhong et al., "MemoryBank" (AAAI 2024)** — Ebbinghaus-decay where
  reused/important memories are reinforced and neglected ones fade.

  **Design consequence:** persistence should be *earned* — score decisions by stakes, reuse, and
  novelty; only high-scorers stay hot. (Currently approximated by feedback + recency; a dedicated
  importance score is the next increment.)

### 9c. Forgetting is a feature — demote, don't delete (`forget.go`, shipped)
- **Bjork & Bjork, "A New Theory of Disuse" (1992).** Two independent strengths: **storage** (durable,
  only grows) and **retrieval** (fluctuates with use/disuse). "Forgetting" = low retrievability, not
  erasure. This is the core model behind `retention()`.
- **Richards & Frankland, "The Persistence and Transience of Memory" (*Neuron*, 2017).** Active
  forgetting is *adaptive* — shedding outdated detail improves memory-guided decisions and prevents
  overfitting.
- **Anderson, Bjork & Bjork, "Remembering Can Cause Forgetting" (*JEP:LMC*, 1994).** Retrieving some
  items *suppresses* competitors — keeping everything actively *hurts* recall of what matters.
- **ML analogues:** **McCloskey & Cohen, catastrophic interference (1989)** (the failure mode of
  *unmanaged* forgetting); **LRU/LFU eviction** as engineered forgetting in bounded memory systems.

  **Design consequence — implemented:** `retention()` computes live retrievability from age-since-use,
  reinforced by helpful/used feedback, eroded by wrong/not-relevant. Below `forget.floor` a decision is
  **demoted out of hot recall but kept in the store** (Bjork: retrievability low, storage intact) and
  **revives when reinforced**. `POST /consolidate` reports hot vs cold with reasons; nothing is ever
  deleted. Honest-scope: at scale, cold decisions should be *archived out of the hot index* rather than
  re-scored every recall — current impl re-scores all (fine at this size).

---

## Provenance & honesty

All papers above were verified against primary or authoritative sources (arXiv, JMLR, DBLP,
Crossref, Springer, Wikipedia). Where a claim rests on canonical citation knowledge rather than a
fetched page, or on a synthesis of several papers rather than a single theorem, it is flagged inline
as an *honest-scope note*. Two references reported from established citation knowledge (page numbers
not re-fetched): Chow (1970) and the *Dataset Shift in Machine Learning* volume (Quiñonero-Candela
et al., MIT Press, 2009) — both real and canonical; re-verify exact pages before precise citation.

The design's core thesis — *capture the why, retrieve-and-advise, abstain under uncertainty, and
refuse to fake a causal correctness signal* — is built from these published results. The synthesis
is ours; the pieces are real.

### Reference index

| # | Reference | Backs |
|---|---|---|
| Pearl 2009 | *Causality* (Cambridge UP) | §1 confounding / back-door |
| Heckman 1979 | Sample Selection Bias (*Econometrica*) | §1 counterfactual missing |
| Imbens & Angrist 1994 | LATE (*Econometrica*) | §1 exogenous variation |
| Angrist & Pischke 2009 | *Mostly Harmless Econometrics* | §1 IV practice |
| Rao 1945 / Cramér 1946 | Cramér–Rao bound | §1 non-identifiability |
| Chow 1970 | Optimum reject tradeoff (*IEEE TIT*) | §2 precision floor |
| El-Yaniv & Wiener 2010 | Selective classification (*JMLR*) | §2 risk–coverage |
| Cortes, DeSalvo & Mohri 2016 | Learning with Rejection (*ALT*) | §2 joint reject |
| Geifman & El-Yaniv 2017/2019 | Selective Class. / SelectiveNet | §2 modern abstention |
| Vovk, Gammerman & Shafer 2005 | *Algorithmic Learning in a Random World* | §3 conformal |
| Angelopoulos & Bates 2021 | Gentle Intro to Conformal (arXiv:2107.07511) | §3 conformal path |
| Angelopoulos et al. 2022 | Conformal Risk Control (arXiv:2208.02814) | §3 loss control |
| Abbasi Yadkori et al. 2024 | Conformal Abstention for LLMs (arXiv:2405.01563) | §3 abstain guarantee |
| Shimodaira 2000 | Covariate shift (*JSPI*) | §4 biased eval |
| Sugiyama et al. 2007 | IWCV (*JMLR*) | §4 biased CV |
| Lewis et al. 2020 | RAG (arXiv:2005.11401) | §5 retrieve-then-advise |
| Guu et al. 2020 | REALM (arXiv:2002.08909) | §5 retrieval LM |
| Karpukhin et al. 2020 | DPR (arXiv:2004.04906) | §6 asymmetric encoders |
| Wang et al. 2022 | E5 (arXiv:2212.03533) | §6 query/passage prefixes |
| Izacard et al. 2021 | Contriever (arXiv:2112.09118) | §6 dense retrieval |
| Nussbaum et al. 2024 | Nomic Embed (arXiv:2402.01613) | §6 nomic-embed-text |
| Park et al. 2023 | Generative Agents (arXiv:2304.03442) | §7 memory stream · §9b importance gate |
| Packer et al. 2023 | MemGPT (arXiv:2310.08560) | §7 tiered memory |
| Liu et al. 2023 | Lost in the Middle (arXiv:2307.03172) | §8 inject few |
| Tulving & Thomson 1973 | Encoding specificity (*Psych. Review*) | §9a context = cue |
| Godden & Baddeley 1975 | Context-dependent memory (*Br. J. Psych.*) | §9a context prior |
| Bower 1981 | Mood and memory (*American Psychologist*) | §9a state-dependent |
| Gao et al. 2025 | CDMem (NAACL) | §9a context-keyed agent memory |
| McClelland, McNaughton & O'Reilly 1995 | Complementary Learning Systems (*Psych. Review*) | §9b consolidation |
| Squire & Alvarez 1995 | Standard consolidation (*Curr. Opin. Neurobiol.*) | §9b consolidation |
| Frey & Morris 1997 | Synaptic tagging & LTP (*Nature*) | §9b salience gates keeping |
| Diekelmann & Born 2010 | Memory function of sleep (*Nat. Rev. Neurosci.*) | §9b consolidation |
| Castel et al. 2011 | Value-directed remembering (*Dev. Psych.*) | §9b importance |
| McGaugh 2000 | A century of consolidation (*Science*) | §9b arousal = importance |
| Schaul et al. 2015 | Prioritized Experience Replay (arXiv:1511.05952) | §9b importance replay |
| Zhong et al. 2024 | MemoryBank (AAAI) | §9b/9c decay + reinforce |
| Bjork & Bjork 1992 | A New Theory of Disuse | §9c storage vs retrieval strength |
| Richards & Frankland 2017 | Persistence & Transience (*Neuron*) | §9c adaptive forgetting |
| Anderson, Bjork & Bjork 1994 | Remembering Can Cause Forgetting (*JEP:LMC*) | §9c keeping-all hurts |
| McCloskey & Cohen 1989 | Catastrophic interference | §9c unmanaged forgetting |
