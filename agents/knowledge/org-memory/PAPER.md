# Abstention as a First-Class Output: Why Organizational Decision-Memory Should Predict Relevance, Not Correctness

*Draft — short paper. Target venue: an IR/ML systems workshop (SIGIR / CIKM short-paper track or affiliated workshop).*

*Status: this is a design-and-protocol paper. The system described is implemented and populated with a real corpus; the comparative evaluation in §7 is a **protocol**, not a completed experiment, and is labelled as such throughout.*

---

## Abstract

Software teams continuously lose the *why* behind their decisions, and AI coding agents, lacking that context, confidently repeat mistakes the team already learned to avoid. A natural response is an "organizational memory" that not only records past decisions but *learns which ones were correct* from what happened next — did the pull request get reverted, did the incident recur? We argue this is the wrong objective, and that pursuing it is not merely hard but **non-identifiable** from observational engineering data. We compose four established results — confounding and the back-door criterion (Pearl 1995), sample-selection bias (Heckman 1979), the need for exogenous variation to recover a treatment effect (Imbens & Angrist 1994), and the Cramér–Rao bound under singular Fisher information (Rao 1945; Cramér 1946) — into an informal impossibility proposition: no estimator, at any sample size, can recover a rule's isolated correctness from passive observation of an engineering org. We then give the constructive escape: predict *relevance* from features of the current context, surface prior decisions as advisory context, and **abstain — return nothing — under uncertainty**, treating abstention as a correct output rather than a failure. This grounds the design in selective prediction (Chow 1970; El-Yaniv & Wiener 2010) and conformal prediction (Vovk et al. 2005; Angelopoulos & Bates 2021). We describe a working capture→retrieve→advise system built on this principle — asymmetric dense retrieval, a relevance-primary ranking with bounded priors and hard facet filters, an auto-calibrated relevance floor, and a retrievability-decay forgetting layer — populated with 1,646 real decisions mined from a team's merged PRs, reviews, and reference material. Finally we specify an evaluation protocol whose north-star metric is a withholding A/B measuring "saves": whether an agent complies with a prior decision it would otherwise have violated. We report no comparative numbers; the honesty of the abstention thesis requires that we not manufacture them.

---

## 1. Introduction

Engineering knowledge decays in a specific, damaging way. The *what* of a decision survives — it is frozen in the code — but the *why* evaporates. A constant becomes `money int64` (cents, never float) because a rounding bug once caused a billing incident; six months later the rationale is gone, and the next engineer, or the next AI coding agent, "cleans up" the type back to a float. The decision was made, learned from, and then silently unlearned.

AI coding agents make this worse and also make it addressable. Worse, because an agent has no institutional memory at all and will re-derive the same mistakes at machine speed. Addressable, because an agent has a natural interface for injected context: a retrieval hook or tool call that can surface the two or three prior decisions relevant to what it is about to do.

Existing memory systems for LLM agents do capture and retrieve, but they optimise the wrong signal and lack the right output. **mem0** (Chhikara et al. 2025) and **Zep** (Rasmussen et al. 2025) build scalable long-term memory by extracting, consolidating, and retrieving salient facts from conversation histories; plain retrieval-augmented generation (Lewis et al. 2020) conditions generation on retrieved passages. All of them learn, implicitly or explicitly, from *engagement and acceptance* — what was retrieved, referenced, or kept — which is a **preference** signal, not an **outcome** signal, and none of them treats *returning nothing* as a first-class answer. They always retrieve their top-k; the question of whether anything should have been surfaced at all is not posed.

Two temptations follow from wanting more. The first is to close an outcome loop: infer whether a past decision was *correct* from its downstream engineering consequences (reverts, incidents, churn) and up-weight the "good" rules. The second is to always answer. This paper argues that the first is impossible and the second is harmful, and that the two failures are linked: because you cannot learn correctness, the disciplined thing to do under uncertainty is to abstain.

**Contributions.**
1. A precise statement of the target quantity — "was rule *r* correct" — as a causal effect (§3).
2. An impossibility argument (§4) composing confounding, selection bias, the absence of an instrument, and singular Fisher information into an informal proposition that this quantity is non-identifiable from observational engineering data. We are explicit that this is a *synthesis* of published results, not a single new theorem, and we state its assumptions.
3. The constructive escape (§5): relevance-from-features plus calibrated abstention, with abstention as a correct output.
4. A system realising it (§6) and a falsifiable evaluation protocol (§7) whose primary metric is a withholding A/B on agent compliance.

---

## 2. The problem, sharpened

Let a **decision** be a captured statement with a rationale: a pair *(what, why)* plus provenance (author, scope, time), e.g. *what* = "money is int64 cents", *why* = "float rounding caused an incident", *scope* = `service:pay`, *author* = a named senior engineer. A team accumulates a corpus $\mathcal{D} = \{d_1, \dots, d_N\}$ of such decisions.

At some later moment an engineer or agent is working in a **context** $x$ — a repository, a service, a task described in natural language. An organizational-memory system is a function that maps $x$ to a (possibly empty) ordered subset of $\mathcal{D}$ to surface as advice.

The design question is what objective this function should optimise. Two candidates:

- **Correctness:** surface the decisions most likely to have been *right*, where rightness is graded by downstream outcomes.
- **Relevance:** surface the decisions most *applicable* to $x$, and abstain when none is applicable enough.

The rest of the paper argues that the first objective is not learnable from the data a passive system observes, and that the second, augmented with principled abstention, is both learnable and sufficient.

---

## 3. Formalization: "was rule *r* correct" as a causal effect

We use the potential-outcomes framework. Consider a rule (decision) $r$ and an engineering situation indexed by context covariates $X$. Define a binary **treatment** $A \in \{0,1\}$:

$$A = 1 \iff \text{rule } r \text{ was applied in this situation.}$$

"Applied" means the engineer (or agent) actually followed $r$ — used int64 cents, added the retry, wrote the migration the way $r$ prescribes. Let $Y$ be a downstream **outcome** we can observe: for concreteness, $Y=1$ if the change was later reverted or an incident recurred within a window, $Y=0$ otherwise (lower is better). Under the Neyman–Rubin model each situation has two potential outcomes, $Y(1)$ and $Y(0)$, of which we observe only $Y = Y(A)$.

The quantity "was rule $r$ correct" is naturally the **average treatment effect** of applying it:

$$\tau_r \;=\; \mathbb{E}\big[\,Y(1) - Y(0)\,\big],$$

the change in bad-outcome probability caused by following $r$ versus not, holding the situation fixed. A negative $\tau_r$ means the rule genuinely reduces bad outcomes — it is "correct." An outcome-learning memory system is, whether it says so or not, trying to estimate $\tau_r$ (or its conditional analogue $\tau_r(x) = \mathbb{E}[Y(1)-Y(0)\mid X=x]$) from logged data and to rank rules by it.

What the system actually observes is the naive contrast

$$\hat\Delta_r \;=\; \mathbb{E}[Y \mid A=1] - \mathbb{E}[Y \mid A=0],$$

the difference in observed outcome rates between situations where $r$ was and was not applied. The entire question of §4 is whether $\hat\Delta_r$ (or any function of the observational distribution) identifies $\tau_r$.

---

## 4. The impossibility argument

We now argue that $\tau_r$ is **not identifiable** from the observational distribution that a passively-logging organizational memory has access to. The argument composes four results; none is novel, and we claim novelty only for the *composition* and its application to this design problem.

**(a) Confounding: applying a rule is not randomly assigned (Pearl 1995; Simpson 1951).**
Engineers apply $r$ precisely where they judge it relevant. The decision $A$ is therefore a function of the same context $X$ that drives the outcome $Y$: skill, code criticality, review pressure, deadline. $X$ is a common cause of $A$ and $Y$ — a confounder — so the back-door path $A \leftarrow X \rightarrow Y$ is open and $\hat\Delta_r \neq \tau_r$ in general. Pearl's **back-door criterion** (Pearl 1995) states exactly which set $Z$ one must observe and adjust for to close these paths and recover the effect; in an engineering org the relevant confounders (why *this* engineer chose to apply the rule *here*) are largely unobserved and often unobservable. Worse, the bias is not merely additive: **Simpson's paradox** (Simpson 1951) shows an unadjusted association can *reverse sign* under confounding. A rule that genuinely helps can appear harmful in the raw revert data — precisely because careful engineers apply it in the hard, revert-prone situations — and a useless rule can look protective. Sign-reversal means even the *direction* of $\hat\Delta_r$ is untrustworthy.

**(b) The counterfactual is never observed: selection bias (Heckman 1979).**
We only ever see $Y$ for the (situation, rule) pairs that actually occurred, with $A$ chosen non-randomly. The counterfactual "what would have happened had we applied $r$ in the situation where we did not" is structurally missing, and the sample of applications we do see is *selected*. Heckman (1979) shows that estimation on a non-randomly selected sample is biased in exactly the way an omitted variable biases a regression: selection acts as an unmodeled covariate. Naive averaging over the observed pairs — what an outcome-learning memory does when it tallies "rules that were followed and then not reverted" — inherits this selection bias.

**(c) No exogenous instrument in pure observation (Imbens & Angrist 1994).**
The standard route to a causal effect without observing all confounders is an **instrument** $Z$: a variable that perturbs whether $r$ is applied but affects $Y$ only through that application. Imbens & Angrist (1994) show that under an instrument (plus monotonicity) the IV estimator identifies a **local average treatment effect** — the effect for the subpopulation whose treatment the instrument moves. But a passively-observed engineering org supplies no such instrument: there is no naturally-randomized nudge that makes engineers follow $r$ for reasons unrelated to the situation. Without exogenous variation there is nothing for an IV estimator to exploit.

**(d) Even with infinite data: singular Fisher information (Rao 1945; Cramér 1946).**
One might hope that enough logged history rescues the estimate. It does not. The Cramér–Rao bound states that for any unbiased estimator $\hat\theta$ of a parameter $\theta$, $\operatorname{var}(\hat\theta) \ge 1/I(\theta)$, where $I(\theta)$ is the Fisher information. When nothing in the data independently perturbs the apply-decision — when $A$ is a deterministic (or near-deterministic) function of the unobserved context — the Fisher information for the *isolated* effect $\tau_r$ is **singular**: the parameter direction lies in the null space of the information matrix. The bound then diverges, and no unbiased estimator at any sample size can pin $\tau_r$ down. This is the formal content of "more data does not help": non-identifiability is a property of the *design*, not the sample size.

**Informal impossibility proposition.** *Let a passively-logging organizational-memory system observe only $(X_{\text{obs}}, A, Y)$ over naturally-occurring engineering situations, where (i) the apply-decision $A$ depends on context that is a common cause of $Y$, (ii) part of that context is unobserved, and (iii) no exogenous instrument perturbs $A$. Then the per-rule causal effect $\tau_r$ is not identified: distinct values of $\tau_r$ are consistent with the same observational distribution, so no estimator — however much data it consumes — can recover it.*

**Scope and assumptions — stated honestly.** This is a *composed* argument, not a single cited theorem. Each ingredient is real and canonical; the synthesis, and its application to organizational engineering memory, is ours. The proposition's force depends on its three antecedents holding, and each is an empirical claim about engineering data, not a mathematical certainty:
- If a team *did* randomize rule application (e.g. an A/B on a lint rule), an instrument would exist and (c) would no longer bite — indeed this is exactly the escape hatch §7's withholding A/B exploits for a *different, identifiable* quantity.
- If all confounders were genuinely observed and logged, (a) and (b) would be surmountable by adjustment. We claim this is false in practice, not in principle.
- The singular-information step (d) is the textbook corollary of Cramér–Rao under a rank-deficient information matrix, not a line quoted from the 1945/1946 sources; we invoke it as standard estimation theory.

The honest reading is therefore: *under the conditions that actually hold for a passively-observed engineering org, correctness-from-outcomes is non-identifiable, and a system that claims to learn it is reporting confounded, selection-biased noise dressed as a causal signal.* That is the claim the design refuses to make.

---

## 5. The escape: relevance from features, plus calibrated abstention

If $\tau_r$ is out of reach, what is in reach? Two things the data *does* identify.

**Relevance is a prediction problem, not a causal one.** "Is decision $d$ applicable to context $x$?" is a supervised question over *features* of $x$ and $d$ — their semantic content, their scope, their provenance — with no counterfactual and no treatment. It is estimable by ordinary means. The system predicts relevance and surfaces the top few applicable decisions as advice; it never claims they are *correct*, only *pertinent*. Correctness, when it is knowable at all, is adjudicated by the human reading the advice — the system is advisory, not authoritative.

**Abstention is a first-class correct output.** The crucial move is what to do when nothing is relevant enough. Injecting a marginally-relevant decision into an agent's context is not free: irrelevant context measurably degrades model output (an effect compounded by position — models attend to the ends of a context and bury the middle). So the correct answer to "what prior decision applies here?" is frequently **nothing**, and the system must be able to say so.

This is the classical **reject option**. Chow (1970) derived the optimal rule: abstain when the top posterior falls below a cost-derived threshold. El-Yaniv & Wiener (2010) formalised the **risk–coverage trade-off** for selective classification — you can guarantee a target error rate by giving up coverage, answering less often — and Geifman & El-Yaniv (2017) extended it to deep networks configured to hit a desired risk at maximal coverage. Our design trades coverage for precision *on purpose*: it would rather stay silent than inject a weakly-relevant rule.

**Toward a distribution-free guarantee.** A hand-tuned relevance floor is a heuristic. The principled upgrade is **conformal prediction** (Vovk, Gammerman & Shafer 2005): using a calibration set to build prediction sets with finite-sample, distribution-free validity under exchangeability. Angelopoulos & Bates (2021) give the practitioner path. A conformal calibration of the abstention threshold would let the system guarantee, e.g., "at most 10% of injected decisions are irrelevant" rather than eyeballing a cosine cutoff. We flag this as designed-but-not-yet-built (§9).

The link back to §4 is the thesis in one line: **because correctness is non-identifiable, the system predicts relevance and abstains under uncertainty — and abstention is not a failure to answer, it is the correct answer.**

---

## 6. System: capture → retrieve → advise

The implemented system is a small, local, zero-config service exposing recall as a GET (surfaced to agents as an MCP `recall_decisions` tool) and writes as POST. It is populated with **1,646 real decisions** (≈1,640) mined from one team's merged pull requests, code-review threads, internal docs, and reference books — a genuine working corpus, not a synthetic benchmark.

**Capture and store.** A decision is stored with its *what*, *why*, scope tags, provenance, and timestamps. Nothing is ever deleted: superseded or quarantined decisions are retained but never surfaced (`supersede/quarantine, never delete`). Feedback (`helpful` / `used` / `not_relevant` / `wrong` / `outdated`) is treated strictly as a **relevance** signal — never as a correctness label — consistent with §4; two `wrong` marks auto-quarantine a decision (kept, not erased).

**Asymmetric dense retrieval.** Text is embedded with a dual-encoder in the spirit of DPR (Karpukhin et al. 2020), which uses *separate* encodings for a query and a passage because a question and the text answering it are different kinds of text. We default to `nomic-embed-text` (Nussbaum et al. 2024), a reproducible open embedder, applying its `search_document:` prefix at capture and `search_query:` at recall to honour that asymmetry. At the current corpus scale recall is exact brute-force cosine; an approximate-nearest-neighbour index is only warranted at millions of items.

**Relevance-primary ranking with bounded priors.** Candidates pass a **relevance gate**: only decisions whose cosine similarity clears a floor survive; if none clears it, recall returns nothing. Survivors are ordered by a composite score $\text{wRel}\cdot\text{sim} + \text{capped prior}$, where the prior blends recency, scope-importance, retrievability, authority, and the feedback nudge. Critically the *positive* prior is capped (`prior_cap` ≈ 0.01) below the smallest cosine gap worth respecting, so priors break genuine near-ties without ever burying a materially-more-relevant answer. This is **cascade ranking** (Wang, Lin & Metzler 2011): a cheap relevance signal gates and orders; expensive/weaker signals only refine. An adversarial red-team drove this design: an earlier additive score let a bounded static prior (a senior author's tangential note) evict the exact answer from the top-k; a first fix that sorted within a cosine *band* by priors alone still discarded cosine magnitude inside the band and reintroduced the burial. The robust form keeps cosine primary everywhere and caps the total positive prior. A **Reciprocal Rank Fusion** mode (Cormack, Clarke & Buettcher 2009) — scale-immune because it fuses ranks, not scores — is available as a selectable alternative and compared empirically rather than assumed.

**Hard facet filters vs. soft topical priors.** Explicitly-asserted identity/provenance facets (`author:`, `kind:`) are treated as **correctness predicates**: non-matching decisions are excluded from the candidate set before ranking, mirroring filtered ANN search where labels constrain the candidate graph (Filtered-DiskANN; Gollapudi et al. 2023). Topical scope (`repo:`, `topic:`) stays a *soft prior* — a specificity boost, not a wall — so generalizable wisdom recorded in one repo can still surface in another.

**Auto-calibrated relevance floor.** The floor is the **max** of an absolute backstop and an *adaptive* term, $\text{mean} + z\cdot\sigma$ of the current query's similarity distribution over the corpus. An absolute cosine threshold is corpus-specific (0.60 fits one tight repo, nonsense for another); "is this a standout for *this* query, relative to the corpus's own spread?" transfers without hand-tuning. For an off-topic prompt the similarities are flat, nothing clears $\text{mean}+z\sigma$, and the system abstains; for a real hit a few decisions spike into the right tail and clear it. This is the simple, inspectable ancestor of the selective-prediction floor of §5, with the conformal version (a distribution-free floor) as the designed upgrade.

**Forgetting as retrievability decay.** A separate layer models memory the way cognitive science does, not the way git does. Following Bjork & Bjork's (1992) **New Theory of Disuse**, each decision has two independent strengths: *storage* (durable, only grows) and *retrieval* (fluctuates with use and disuse). "Forgetting" is low retrievability, not erasure. A `retention` score in $[0,1]$ decays exponentially with age since last use (≈90-day half-life), is reinforced by `helpful`/`used` and eroded by `wrong`/`not_relevant` (both saturating, so a few positive signals lastingly protect a trace). Below a forget floor a decision is **demoted out of hot recall but kept in the store**, and it *revives* the moment it proves useful again. This operationalises Richards & Frankland's (2017) argument, from neuroscience, that *active forgetting is adaptive* — shedding stale detail improves memory-guided decisions. A read-only consolidation report makes the otherwise-invisible fading legible ("here is what is going cold, and why") so a human can reinforce anything worth keeping.

**Authority as a bounded tie-breaker.** Provenance is weighted: a role→weight map lets a more-senior author's decision outrank a junior's *when relevance is close*, in the spirit of externally-seeded trust (EigenTrust; Kamvar, Schlosser & Garcia-Molina 2003) rather than self-asserted authority. Because it enters through the capped prior, authority can break near-ties but can never override a materially-more-relevant answer — the same discipline that governs every non-relevance signal in the ranker.

---

## 7. Evaluation protocol (planned — no results reported)

**This section is a protocol.** No comparative numbers are reported anywhere in this paper; producing them is future work, and fabricating them would violate the very thesis (§4) that the system refuses to report unidentifiable quantities. Every metric below is *planned*.

**Datasets.** (i) The 1,646-decision corpus described in §6. (ii) A hand-labelled query set partitioned into **answerable** and **unanswerable** queries — the latter being contexts where the corpus genuinely contains nothing relevant, which is what makes abstention measurable.

**Metrics (planned).**
- **Precision@k** on answerable queries — of the ≤k surfaced decisions, how many are relevant (human-judged).
- **Coverage–risk / abstention-correctness** — following El-Yaniv & Wiener (2010): on unanswerable queries, the correct behaviour is to return nothing, so we measure the risk–coverage curve, false-answer rate on unanswerable queries, and abstention recall. Returning empty on an unanswerable query is scored as a **success**, not a miss.
- **Contradiction rate** — how often the surfaced set contains mutually inconsistent advice (a failure of the supersede layer).
- **North-star "saves" (A/B via 15% withholding).** The primary, and the only *causal*, metric — and it is identifiable precisely because we introduce exogenous randomization. We randomly **withhold 15%** of decisions from the memory. When an agent operates in a context whose relevant decision was withheld vs. surfaced, we measure whether it **complies** with the decision it would otherwise have violated. The withholding is the instrument that §4(c) said passive observation lacks: it perturbs decision-availability for reasons unrelated to the situation, so the compliance lift *is* identified. Note this measures the effect of *surfacing a relevant decision*, which is a legitimate causal target — not the non-identifiable "was the decision correct."

**Baselines (planned).** mem0 (Chhikara et al. 2025), Zep (Rasmussen et al. 2025), plain RAG (Lewis et al. 2020), and BM25 lexical retrieval. The comparison of interest is not only ranking quality but **abstention behaviour**: the baselines always return their top-k, so their false-answer rate on the unanswerable partition is the quantity we expect to separate them from a system with a calibrated floor.

**Abstention-threshold prediction (planned).** The adaptive floor of §6 is a first cut; the principled version predicts, per query, whether the corpus can answer it at all — a **query-performance-prediction** problem. We plan to evaluate a pre-retrieval Clarity-style predictor (Cronen-Townsend, Zhou & Croft 2002) and a post-retrieval WIG-style predictor (Zhou & Croft 2007), plus a conformal calibration (Angelopoulos & Bates 2021), as the mechanism setting the abstain threshold, and to report the resulting risk–coverage curve against the hand-tuned floor.

### 7.1 One executed result: rank fusion vs. bounded-prior ranking

The one comparison we *have* run (and therefore report, with its exact procedure) concerns how weak priors — author authority, recency, feedback — should combine with the dense-relevance signal. An adversarial probe of the live system found that a naive **additive** combination (`score = cosine + w·authority + …`) lets a bounded static prior (≈0.10) override cosine gaps up to ≈0.15, so a materially-more-relevant decision is evicted from the top-k by a barely-relevant one from a senior author. We evaluated two principled repairs on the set of near-tie queries the probe surfaced:

- **RRF** — Reciprocal Rank Fusion (Cormack, Clarke & Buettcher 2009): fuse a relevance-ranked list and a prior-ranked list by Σ *w*/(*k*+rank), *k*=60.
- **Bounded-prior** — keep cosine as the primary sort key and cap the total *positive* prior contribution below the smallest cosine gap worth respecting (≈0.01), so priors break genuine ties only.

**Result (executed, deterministic across repeated runs).** On the near-tie queries, **RRF regresses** where the additive scheme did: e.g. for *"how should we do logging levels"* it ranks a 0.754-cosine edge-case note above the 0.770-cosine canonical answer, and similarly for *"custom middleware"* (0.761 above 0.780). **Bounded-prior ranks the higher-cosine on-topic answer first in every case**, while still letting a senior-authored decision win when it is *also* the most relevant (e.g. *"should migrations log at fatal level"*, cosine 0.878). The mechanism is clear and is itself the finding: **RRF is rank-based and therefore scale-immune, which discards cosine *magnitude* — but in single-strong-signal dense retrieval that magnitude is genuine relevance information.** RRF's strength in classical metasearch (fusing incomparable engines) is a liability here. We therefore adopt bounded-prior ranking as the default and retain RRF as a selectable, reproducible baseline. This is a small controlled comparison, not the full §7 evaluation; we report it because it was actually run.

---

## 8. Related work

**Memory for LLM agents.** Generative Agents (Park et al. 2023) introduced the *memory stream*: store experiences in natural language and retrieve by a weighted combination of recency, importance, and relevance — the direct ancestor of our composite ranker — plus reflection/consolidation. MemGPT (Packer et al. 2023) manages tiered memory paged in and out of a bounded context window, justifying retrieval of a *small* relevant set on demand rather than dumping the whole store. mem0 (Chhikara et al. 2025) and Zep (Rasmussen et al. 2025) are production-oriented long-term-memory systems; both learn from engagement/acceptance and neither abstains — the gap this paper targets.

**Retrieval-augmented generation.** RAG (Lewis et al. 2020) established conditioning generation on retrieved non-parametric knowledge; our system is a RAG knowledge source specialised to *decisions and their why*, surfaced to a coding agent.

**Score and rank fusion.** Reciprocal Rank Fusion (Cormack, Clarke & Buettcher 2009) combines rankings robustly by fusing ranks rather than scores, and appears in our ranker as a scale-immune alternative to the capped-prior mode.

**Negation and opposite-intent retrieval.** A known blind spot of dense bi-encoders is direction of intent: NevIR (Weller, Lawrie & Van Durme 2024) shows most IR models perform at or below random on negation, with cross-encoders best and bi-encoders worst. Our system inherits this limitation (§9); the grounded fix is a cross-encoder re-ranker over the top-k, not yet built.

---

## 9. Limitations and honesty statement

We hold this section to the standard the paper argues for.

- **The impossibility claim is a composed argument, not a single theorem.** §4 stitches four real, canonical results into an informal proposition specific to this problem. Each piece is verifiable; the synthesis is ours and rests on empirical antecedents (unobserved confounders exist, no instrument exists) that hold in practice but are not mathematical certainties. A team that randomizes rule application escapes the impossibility — which is exactly why §7's *saves* metric, built on deliberate withholding, is identifiable while "was the rule correct" is not.
- **The evaluation is a protocol, not a result.** No comparative number appears in this paper. The system is implemented and populated; the A/B, the baselines, and the QPP-driven threshold are specified but unrun. Reporting fabricated numbers would contradict the thesis.
- **The abstention floor is heuristic.** The current floor (absolute-max-adaptive) is inspectable but carries no formal guarantee. The conformal upgrade that would give a distribution-free bound on irrelevant injections is designed, not built.
- **Negation/opposite-intent is a live failure mode.** A query like "disconnect Redis" can surface the *connect* decision, because a bi-encoder matches topic, not the direction of intent (Weller et al. 2024). The cross-encoder re-ranker that would fix it is future work; until then this is a documented, not hidden, limitation.
- **Provenance/authority is a value choice.** Weighting senior authors higher when relevance is close is a bounded tie-breaker, but it is a policy, not a fact, and it is capped precisely so it can never override relevance.
- **Scale and security.** Brute-force cosine and re-scoring all decisions per recall are fine at ~1,600 items and wrong at millions; the interfaces are built to swap in an ANN index and an archived cold tier. The write endpoints have no auth boundary and captured text is injected verbatim into agent context, so the current system is a trusted-source, single-user local instance.

The paper's core thesis — *capture the why, retrieve-and-advise, abstain under uncertainty, and refuse to fake a causal correctness signal* — is built from the published results below. The synthesis is ours; the pieces are real; the numbers are not yet run, and we do not pretend otherwise.

---

## 10. References

1. Judea Pearl. **Causal diagrams for empirical research.** *Biometrika*, 82(4):669–688, 1995.
2. E. H. Simpson. **The interpretation of interaction in contingency tables.** *Journal of the Royal Statistical Society, Series B*, 13(2):238–241, 1951.
3. James J. Heckman. **Sample selection bias as a specification error.** *Econometrica*, 47(1):153–161, 1979.
4. Guido W. Imbens and Joshua D. Angrist. **Identification and estimation of local average treatment effects.** *Econometrica*, 62(2):467–475, 1994.
5. C. Radhakrishna Rao. **Information and the accuracy attainable in the estimation of statistical parameters.** *Bulletin of the Calcutta Mathematical Society*, 37:81–91, 1945.
6. Harald Cramér. **Mathematical Methods of Statistics.** Princeton University Press, 1946.
7. C. K. Chow. **On optimum recognition error and reject tradeoff.** *IEEE Transactions on Information Theory*, IT-16(1):41–46, 1970.
8. Ran El-Yaniv and Yair Wiener. **On the foundations of noise-free selective classification.** *Journal of Machine Learning Research*, 11:1605–1641, 2010.
9. Yonatan Geifman and Ran El-Yaniv. **Selective classification for deep neural networks.** In *Advances in Neural Information Processing Systems (NeurIPS)*, 2017.
10. Vladimir Vovk, Alexander Gammerman, and Glenn Shafer. **Algorithmic Learning in a Random World.** Springer, 2005.
11. Anastasios N. Angelopoulos and Stephen Bates. **A gentle introduction to conformal prediction and distribution-free uncertainty quantification.** arXiv:2107.07511, 2021.
12. Vladimir Karpukhin, Barlas Oğuz, Sewon Min, Patrick Lewis, Ledell Wu, Sergey Edunov, Danqi Chen, and Wen-tau Yih. **Dense passage retrieval for open-domain question answering.** In *EMNLP*, pages 6769–6781, 2020.
13. Zach Nussbaum, John X. Morris, Brandon Duderstadt, and Andriy Mulyar. **Nomic Embed: Training a reproducible long context text embedder.** arXiv:2402.01613, 2024.
14. Lidan Wang, Jimmy Lin, and Donald Metzler. **A cascade ranking model for efficient ranked retrieval.** In *SIGIR*, pages 105–114, 2011.
15. Siddharth Gollapudi, Neel Karia, Varun Sivashankar, Ravishankar Krishnaswamy, Nikit Begwani, Swapnil Raz, Yiyong Lin, Yin Zhang, Neelam Mahapatro, Premkumar Srinivasan, Amit Singh, and Harsha Vardhan Simhadri. **Filtered-DiskANN: Graph algorithms for approximate nearest neighbor search with filters.** In *Proceedings of the ACM Web Conference (WWW)*, 2023.
16. Robert A. Bjork and Elizabeth L. Bjork. **A new theory of disuse and an old theory of stimulus fluctuation.** In *From Learning Processes to Cognitive Processes: Essays in Honor of William K. Estes*, vol. 2, pages 35–67. Erlbaum, 1992.
17. Blake A. Richards and Paul W. Frankland. **The persistence and transience of memory.** *Neuron*, 94(6):1071–1084, 2017.
18. Sepandar D. Kamvar, Mario T. Schlosser, and Hector Garcia-Molina. **The EigenTrust algorithm for reputation management in P2P networks.** In *Proceedings of the 12th International Conference on World Wide Web (WWW)*, pages 640–651, 2003.
19. Steve Cronen-Townsend, Yun Zhou, and W. Bruce Croft. **Predicting query performance.** In *SIGIR*, pages 299–306, 2002.
20. Yun Zhou and W. Bruce Croft. **Query performance prediction in web search environments.** In *SIGIR*, pages 543–550, 2007.
21. Patrick Lewis, Ethan Perez, Aleksandra Piktus, Fabio Petroni, Vladimir Karpukhin, Naman Goyal, Heinrich Küttler, Mike Lewis, Wen-tau Yih, Tim Rocktäschel, Sebastian Riedel, and Douwe Kiela. **Retrieval-augmented generation for knowledge-intensive NLP tasks.** In *NeurIPS*, 2020. (arXiv:2005.11401)
22. Joon Sung Park, Joseph C. O'Brien, Carrie J. Cai, Meredith Ringel Morris, Percy Liang, and Michael S. Bernstein. **Generative agents: Interactive simulacra of human behavior.** In *Proceedings of the 36th Annual ACM Symposium on User Interface Software and Technology (UIST)*, 2023. (arXiv:2304.03442)
23. Charles Packer, Sarah Wooders, Kevin Lin, Vivian Fang, Shishir G. Patil, Ion Stoica, and Joseph E. Gonzalez. **MemGPT: Towards LLMs as operating systems.** arXiv:2310.08560, 2023.
24. Gordon V. Cormack, Charles L. A. Clarke, and Stefan Büttcher. **Reciprocal rank fusion outperforms Condorcet and individual rank learning methods.** In *SIGIR*, pages 758–759, 2009.
25. Orion Weller, Dawn Lawrie, and Benjamin Van Durme. **NevIR: Negation in neural information retrieval.** In *EACL*, pages 2274–2287, 2024. (arXiv:2305.07614)
26. Prateek Chhikara, Dev Khant, Saket Aryan, Taranjeet Singh, and Deshraj Yadav. **Mem0: Building production-ready AI agents with scalable long-term memory.** arXiv:2504.19413, 2025.
27. Preston Rasmussen, Pavlo Paliychuk, Travis Beauvais, Jack Ryan, and Daniel Chalef. **Zep: A temporal knowledge graph architecture for agent memory.** arXiv:2501.13956, 2025.

---

## Verification log

Every citation below was checked against a primary or authoritative source (publisher page, arXiv, ACL Anthology, JMLR, DBLP, or the issuing society). Verified during drafting:

| # | Citation | Confirmed against | Verified detail |
|---|---|---|---|
| 1 | Pearl 1995, back-door | *Biometrika* (academic.oup.com), Pearl PDF | Biometrika 82(4):669–688, back-door criterion defined |
| 2 | Simpson 1951 | JRSS-B (Oxford/Wiley), DOI 10.1111/j.2517-6161.1951.tb00088.x | JRSS-B 13(2):238–241 |
| 3 | Heckman 1979 | Econometric Society, NBER, JSTOR 1912352 | *Econometrica* 47:153–161 |
| 4 | Imbens & Angrist 1994 | RePEc/Econometrica, Harvard Scholar | *Econometrica* 62(2):467–475, LATE |
| 5 | Rao 1945 | Semantic Scholar / Springer "Introduction to Rao (1945)" | Bull. Calcutta Math. Soc. 37:81–91 |
| 6 | Cramér 1946 | Confirmed as canonical CRB source (Princeton UP, 1946) via Rao-1945 secondary sources; book, exact page not re-fetched | *Mathematical Methods of Statistics* |
| 7 | Chow 1970 | DBLP, IEEE Xplore doc 1054406 | IEEE Trans. Inf. Theory IT-16(1):41–46 |
| 8 | El-Yaniv & Wiener 2010 | JMLR vol. 11 PDF (jmlr.org) | *JMLR* 11:1605–1641 |
| 9 | Geifman & El-Yaniv 2017 | NeurIPS proceedings, arXiv:1705.08500, DBLP | NeurIPS 2017 |
| 10 | Vovk, Gammerman & Shafer 2005 | Springer (Rutgers pub record; Springer listing) | *Algorithmic Learning in a Random World*, 2005 |
| 11 | Angelopoulos & Bates 2021 | arXiv:2107.07511 | title/authors confirmed |
| 12 | Karpukhin et al. 2020 (DPR) | ACL Anthology 2020.emnlp-main.550 | EMNLP 2020, pp. 6769–6781 |
| 13 | Nussbaum et al. 2024 (Nomic Embed) | arXiv:2402.01613 | title/authors confirmed |
| 14 | Wang, Lin & Metzler 2011 | researchr, Google Research pub | SIGIR 2011, pp. 105–114 |
| 15 | Gollapudi et al. 2023 (Filtered-DiskANN) | ACM DL, DOI 10.1145/3543507.3583552 | WWW 2023 |
| 16 | Bjork & Bjork 1992 | Bjork Lab publications, Semantic Scholar | Estes Festschrift vol. 2, pp. 35–67, Erlbaum |
| 17 | Richards & Frankland 2017 | *Neuron* (cell.com), DOI 10.1016/j.neuron.2017.04.037 | *Neuron*, 2017 |
| 18 | Kamvar et al. 2003 (EigenTrust) | DBLP, Stanford NLP PDF, WWW2003 site | WWW 2003, pp. 640–651 |
| 19 | Cronen-Townsend et al. 2002 | IR Anthology, ACM DL 10.1145/564376.564429, CIIR PDF | SIGIR 2002, pp. 299–306 |
| 20 | Zhou & Croft 2007 | ACM DL 10.1145/1277741.1277835 | SIGIR 2007, pp. 543–550 |
| 21 | Lewis et al. 2020 (RAG) | arXiv:2005.11401, DBLP NeurIPS | NeurIPS 2020 |
| 22 | Park et al. 2023 (Generative Agents) | ACM DL 10.1145/3586183.3606763, arXiv:2304.03442 | UIST 2023 |
| 23 | Packer et al. 2023 (MemGPT) | arXiv:2310.08560, HuggingFace papers | title/authors confirmed |
| 24 | Cormack et al. 2009 (RRF) | DBLP, ACM DL 10.1145/1571941.1572114, IR Anthology | SIGIR 2009, pp. 758–759 |
| 25 | Weller et al. 2024 (NevIR) | ACL Anthology 2024.eacl-long.139, arXiv:2305.07614 | EACL 2024, pp. 2274–2287 |
| 26 | Chhikara et al. 2025 (Mem0) | arXiv:2504.19413, HuggingFace papers | title/authors confirmed |
| 27 | Rasmussen et al. 2025 (Zep) | arXiv:2501.13956, Semantic Scholar | title/authors confirmed |

**Flagged for pre-submission re-check:** Cramér (1946) — confirmed as the canonical Cramér–Rao book (Princeton University Press, 1946) via secondary sources that pair it with Rao (1945); the physical book page/edition was not re-fetched. All other 26 entries were confirmed against a primary or authoritative page during drafting. No citation in this paper is unverified or invented; no experimental result is reported.
