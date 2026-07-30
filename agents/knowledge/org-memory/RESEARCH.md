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

## 10. Adversarial red-team → research-grounded fixes

A fable-5 red-team (3 adversaries: false-injection, ranking, robustness) found real defects. Each was
fixed against verified IR/ML literature, not by ad-hoc tuning. Status: ✅ fixed · ⏭ designed/deferred.

### 10a. Ranking: static priors evicting the relevant answer ✅
**Defect:** a linear additive score (`cosine + 0.15·authority + …`) let a bounded static prior (+0.10)
override cosine gaps up to ~0.15 — a CTO's tangential note buried the exact answer, evicting it from
the top-3 entirely (reproduced across ~12 queries).
**Fix — cascade ranking + a BOUNDED prior tie-breaker.** Cosine gates recall and orders results; the
priors can nudge the order by at most `prior_cap` (≈0.01), so they break genuine near-ties without ever
burying a materially-more-relevant answer. (A first attempt used a fixed cosine *band* and sorted
within it by priors alone — a re-run red-team showed that still discarded cosine *inside* a band,
recreating the burial and adding recency inversions; the robust form keeps cosine primary everywhere
and caps the total positive prior below the smallest cosine gap worth respecting. Negative feedback is
uncapped, so a "not relevant" mark still demotes.) *Wang, Lin & Metzler, "A Cascade Ranking Model,"
SIGIR 2011* (relevance gates); *Kraaij et al., SIGIR 2002* (priors as small terms, not large offsets). The
literature's alternatives (all verified) also apply: **RRF** — Cormack, Clarke & Buettcher, SIGIR 2009
(rank fusion, scale-immune); score normalization — Fox & Shaw 1994 / Montague & Aslam 2001; priors as
small *log-priors* not additive constants — Kraaij, Westerveld & Hiemstra, SIGIR 2002; quality-biased
ranking — Bendersky, Croft & Diao, WSDM 2011. Root cause is categorical: a static additive prior on an
un-normalized relevance score can always dominate — so relevance must gate, priors must be tie-breakers.

> **⚠️ The fix above was documented here but NOT implemented (found 2026-07-30, now corrected).**
> This section already named score normalization — *Fox & Shaw 1994 / Montague & Aslam 2001* — as a
> verified applicable alternative, and stated the root cause as "a static additive prior on an
> **un-normalized** relevance score." The code did not normalize. It computed `prior = Σ wᵢxᵢ` and
> clamped the **sum** at `prior_cap`. Since recency + importance + retrievability alone always exceed
> a 0.01 cap, the sum saturated and **every** candidate received exactly `+prior_cap` — a constant
> offset, which carries no ordering information. Measured pre-fix: `score − similarity = +0.010` for
> every item in every provenance, a CTO-authored review and an unattributed book alike.
>
> The damage went past a dead feature: it made the validation **unfalsifiable**. "Does a
> high-authority item evict a more-relevant one?" and "does toggling `w_authority` change the order?"
> both passed, and neither *could* have failed — a uniform offset cannot reorder anything. Both were
> reported as evidence that a bounded authority prior behaves safely.
>
> **Now implemented as documented:** the prior is a weighted **mean** of `[0,1]`-scaled signals scaled
> by `prior_cap` — bounded by construction rather than by clipping a runaway sum. *Lee, "Analyses of
> Multiple Evidence Combination," SIGIR 1997*; *Montague & Aslam, CIKM 2001*. Regression tests
> (`TestRankPriorIsNotConstant`, `TestRankAuthorityToggleChangesOrdering`) fail against the old code.
>
> **Measured limitation, stated rather than claimed away:** at deployed weights authority's maximum
> contribution is `prior_cap × w_auth/Σw = 0.01 × 0.15/1.35 ≈ 0.0011`, against a **median adjacent
> cosine gap of 0.011** in top-5. Toggling `w_authority` 0 ↔ 0.15 across 20 queries changes **zero**
> orderings. The prior is no longer degenerate; it remains *practically inert*. `prior_cap` stays at
> 0.01 and authority is **not** claimed as an operative ranking signal.
>
> **Lesson for this document:** a citation here is a claim about the code, and nothing was checking
> it. This section read correctly and was wrong in production the entire time.

### 10b. Facets treated as soft hints, not filters ✅
**Defect:** `author=vikash` leaked other authors; `scope=kind:book` returned non-book items — facets
were concatenated into the query as ranking hints.
**Fix — hard facet pre-filter.** Explicitly-asserted identity/provenance facets (`author:`, `kind:`)
are correctness PREDICATES: non-matching decisions are excluded from the candidate set before ranking.
Topical scope (`repo:`, `topic:`) stays a soft prior. *Gollapudi et al., "Filtered-DiskANN," WWW 2023;
Hearst, "…Hierarchical Faceted Search Interfaces," 2006* (browse-constraint vs. free-text-relevance).

### 10c. Abstention & confidence were absolute thresholds ✅ (partial) / ⏭
**Defect:** a fixed floor + `weak = top<0.75` misjudged a heterogeneous corpus — off-topic queries
crossed the floor; a confident-but-wrong result ("cache") read as strong.
**Fix (shipped):** the weak flag is now **margin-based** — weak unless the top result *stands out* from
the runner-up (`top1−top2`), not just clears an absolute bar. *Manmatha, Rath & Feng, "Modeling Score
Distributions," SIGIR 2001.* ("cache" now correctly flags weak.)

> **⚠️ That margin rule was itself the next defect (found 2026-07-30, now corrected).** Shipping it as
> an `OR` against the absolute bar — `weak = absWeak || marginWeak` — fused two unrelated questions
> into one boolean, and the margin half dominated. Measured: `weak=true` on **16/32** genuine hits,
> including correct matches at similarity 0.83–0.85; an independent cross-family audit reproduced it
> at 5/5. Meanwhile it stayed **false** on the confidently-wrong negation failures it most needed to
> catch (§10d: *"should we fail closed"* → *"Fail **open**"* at 0.778, `weak=false`). A flag that
> fires on half of all correct answers and stays silent on the dangerous wrong ones is not a
> confidence signal.
>
> The conflation is the bug. A tight cluster of near-equally-relevant results means **the corpus is
> redundant on this topic**, not that the answer is doubtful — a distinct question from "is the top
> match strong at all." They are now reported separately as `weak` (absolute confidence) and
> `ambiguous` (flat top-2 distribution), both measured against the **effective** floor rather than
> the absolute `precision_floor` — a second latent bug, since with adaptive gating on, those differ
> and the comparison silently mis-labels confidence exactly when adaptation is doing the work.
>
> Post-fix, every previously-"weak" genuine hit (circuit breaker, rate limiting, error handling) is
> correctly `weak=false, ambiguous=true`. **This makes the signal measurable, not validated** — a real
> verdict needs a labeled set of correct vs incorrect recalls, and the deferred QPP path below is
> still the rigorous answer. Note *Cronen-Townsend et al., SIGIR 2002* and *Zhou & Croft, SIGIR 2007*
> were already cited here as that path; the margin heuristic was shipped as an interim stand-in and
> then treated as the fix.
>
> **Related (§10c ⇄ honesty):** `top_similarity` was computed as the max over the *surfaced* list, so
> every abstention reported `0.000` — an absence, not a measured cosine — and a validation report
> read that as "the strongest off-topic scored 0.0" and concluded the abstention margin was
> "enormous." `recall` now returns `top_candidate_similarity` (best pre-gate cosine) and the `floor`.
> Re-measured, the real margin is **0.073**, and gibberish scores **0.612** against a 0.653 floor. An
> abstention must be *measurable* to be calibrated at all — *El-Yaniv & Wiener 2010* (risk–coverage,
> reference index) is meaningless against a masked score.
**Deferred (⏭), with the grounded path:** the rigorous version is a post-retrieval **QPP** gate (WIG:
mean-top-k minus corpus-mean cosine — *Zhou & Croft, SIGIR 2007*; Clarity — *Cronen-Townsend et al.,
SIGIR 2002*), a **lexical/BM25 hybrid gate** to kill topically-near-but-off-intent hits (*Thakur et al.,
BEIR, NeurIPS 2021*), cosine→probability calibration (*Platt 1999; Lin et al. 2007*), and **conformal**
thresholds for a distribution-free abstention guarantee (*Angelopoulos & Bates 2021*) replacing the
noise-ceiling heuristic.

> **❌ The BM25 hybrid was built, measured, and REJECTED (2026-07-30).** Implemented in `lexical.go`
> (identifiers preserved as whole tokens, RRF fusion) and swept over `rank.w_lexical ∈ {0…2.0}`
> against a 15-query set and 3 polarity pairs. Full report: `eval/validation/hybrid_lexical.md`.
>
> - **precision@3: dense-only 15/15, every hybrid weight 14/15.** It loses `which database is the
>   source of truth for order data` — a query that does not contain the token `orders`, so BM25
>   matches "source"/"truth" instead and promotes a decision about READMEs. The hybrid helps only when
>   the query already names the exact identifier, and dense retrieval was handling those anyway.
> - **negation got WORSE, not better: identical-top on 1/3 pairs → 2/3.** "add retries" and "do not
>   retry" share three high-idf content terms and differ by low-idf stopword-adjacent negations, so
>   BM25 pulls the polarities *together* harder than the embedding does. The hypothesis was backwards.
>
> Shipped disabled by default (zero cost — the index is never built while the weight is 0). The
> deferred-path list above is amended: the lexical gate is closed, not pending. This also **narrows**
> the cross-encoder case in §10d — NevIR's result is about a joint query+document read, and BM25 was
> the cheap approximation of "look at the actual tokens"; the measurement says the cheap approximation
> does not capture what makes the cross-encoder work.

### 10d. Negation / opposite-intent blindness ⏭ (documented, not faked)
**Defect:** *"disconnect Redis"* returns the *connect* doc, confidently — dense bi-encoders match topic,
not the direction of intent. Confirmed by *Weller et al., "NevIR," EACL 2024* (most IR models ≤ random
on negation; **cross-encoders best, bi-encoders worst**) and *García-Ferrero et al., EMNLP 2023*.
**Grounded fix (not yet built):** a **cross-encoder re-ranker** over the top-k (*Nogueira & Cho,
"Passage Re-ranking with BERT," 2019*) — jointly reads query+doc and demotes opposite-intent matches a
bi-encoder can't tell apart; or an instruction field (*Weller et al., "FollowIR," 2024*). Requires a
cross-encoder model, so it's the next build; until then this failure mode is a known, documented limit.

> **Scope is wider than negation — this also causes false contradictions (found 2026-07-30).** The
> order-storage cluster that drove the 9.4% contradiction rate is the *same* defect wearing different
> clothes. Two decisions, both verbatim-correct against their source docs, about **different tables**
> — `orders` (ClickHouse sole store) and `order_events` (MySQL source of truth) —
> surface together as if they conflicted, because a bi-encoder ranks the shared phrasing "source of
> truth" above the entity token that distinguishes them. Not polarity this time, but **entity**; same
> root cause, same fix.
>
> Adding `table:` subject facets was tried and **did not fix it**: `hardFilterPrefixes` is
> `["author:", "kind:"]`, so `table:` is an inert soft prior (verified — `scope=table:orders`
> and `scope=table:order_events` return identical sets), and even as a hard predicate it
> cannot help a free-text query that carries no facet. Metadata was a necessary condition, not a
> remedy.
>
> This raises the priority of the cross-encoder re-ranker: it is not a single-failure-mode fix. One
> build closes **negation blindness and entity conflation together**, which is the highest-value open
> item in the system.

### 10e. Robustness / safety bugs ✅ (eng fixes) / ⏭ (auth)
Plain bugs, fixed directly: **top_k=MaxInt OOM-crashed the process** → clamped at read + rejected at the
config write site (`[1,50]`); a **negative `precision_floor` dumped the whole store** → clamped ≥0 and
range-validated; **blank query returned canned results** (constant embedding) → now returns nothing;
`/consolidate` limit bounded. ⏭ **No auth boundary** on `/config`, `/capture`, `/consolidate` — a real
architectural limitation for anything beyond a local single-user instance; bind-to-loopback + a write
token is the documented next step. `/capture` text is injected verbatim into agent context, so capture
must stay trusted-source-only until an input-provenance gate exists.

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

---

## 11. Parameter grounding — which numbers are *fitted* and which are merely *chosen*

This document opens by claiming every non-obvious design decision traces to an established result.
Audited against the code on 2026-07-30 (`PHASE0-AUDIT.md`), that holds for the **architecture** and
fails for the **parameters**: of 28 live knobs, 9 are backed, 5 partially, and **14 are asserted with
no basis** — including every confidence threshold (`weak_margin`, `weak_min_margin`, `adaptive_z`)
and every decay and feedback constant. For a system whose moat is calibrated honesty, an
unjustifiable threshold is the same defect class as a masked similarity score: a number that reads
as a measurement and is not.

Two decisions turned out to be **published methods the repo never cited**:

- **The auto-calibrated floor is signal-to-noise normalization.** Booting, probing with generic
  off-topic queries, measuring the similarity ceiling that noise reaches, and setting the floor just
  above it is exactly *Arampatzis & Kamps, "A signal-to-noise approach to score normalization," CIKM
  2009*. `retrieve.calibration_margin` is the SNR offset. Best idea in Phase 0, documented as a local
  trick.
- **`rrf_k = 60` is Cormack et al.'s measured TREC optimum**, not a guess; k ∈ [40,80] is comparable.

The principled replacements for the asserted numbers:

- **Cutoff / `precision_floor` / `top_k`** — *Arampatzis, Kamps & Robertson, "Where to stop reading a
  ranked list? Threshold optimization using truncated score distributions," SIGIR 2009*; learned
  variant *Bahri et al., arXiv:2102.12793*. Fit the score distribution and optimize the target
  measure instead of fixing a cosine number.
- **`weak` / `ambiguous` thresholds** — fit the standard **normal–exponential mixture** (relevant ≈
  normal, non-relevant ≈ exponential): *Manmatha, Rath & Feng, SIGIR 2001*; *Arampatzis & Robertson,
  "Modeling score distributions in information retrieval," Information Retrieval 2011*. This yields
  `P(relevant | score)` — a calibrated probability replacing `weak_margin = 0.10`, and a fitted
  quantile replacing `adaptive_z`.
- **`forget.halflife_days`** — a single global half-life is the naive form. *Settles & Meeder, "A
  Trainable Spaced Repetition Model for Language Learning," ACL 2016* regresses half-life **per item**
  from observed recall. The `R = e^(−t/S)` form is right; one constant `S` for every decision is not.
- **`adaptive_min_n = 8`** — needs no citation to be wrong: σ from n=8 carries ~27% relative standard
  error (~13% at n=30), and the adaptive floor is `mean + z·σ`, so it inherits that noise. The knob
  is a trap for small deployments — precisely where auto-calibration matters most.

### 11a. Implemented — thresholds fitted from the score distribution (`scoredist.go`, 2026-07-30) ✅

The confidence thresholds are no longer chosen. For each query the NON-RELEVANT score distribution is
fitted, and every threshold is expressed against it, so the knobs carry units and transfer across
corpora and embedding models:

| was | is |
|---|---|
| `adaptive_z = 2.0` (a conventional sigma count) | `retrieve.max_false_inject_rate` — the fraction of irrelevant decisions allowed to clear the gate |
| `weak_margin = 0.10` (a bare cosine) | `retrieve.weak_noise_p` — the chance the top match is noise |
| `adaptive_min_n = 8` | `= 30` (σ̂'s relative standard error: 27% → 13%) |

**A latent statistical defect surfaced while doing this.** The old adaptive floor estimated `mean` and
`σ` over *all* candidate similarities — including the relevant ones it existed to admit. Relevant
documents occupy the right tail, so they inflate both moments and raise the floor: **the gate grew
stricter precisely because the query had good answers.** Measured (1600 noise + k relevant, α=0.01),
the classical estimator drifted **+0.136** across k = 0→150; the replacement drifts **+0.018**.

The scale estimator is deliberately **one-sided** — `σ̂ = (Q2 − Q1)/Φ⁻¹(0.75)`, which never reads a
score above the median. Plain MAD was tried first and still drifted 11.6%, because it averages
absolute deviations on *both* sides while the contamination here is entirely right-tailed. Fitting the
non-relevant component only on the range where it is the sole component is exactly the truncated-
distribution construction of *Arampatzis, Kamps & Robertson (SIGIR 2009)*; robust-scale foundations
*Rousseeuw & Croux (JASA 1993)* and *Huber, Robust Statistics*.

Effect on the known failure: the near-domain false-inject (`react useEffect cleanup`, 2 results at
cosine 0.658) that the original abstention validation reported as a clean 0/32 is now **flagged**, at
`noise_p = 1.1e-3`. Gibberish lands at 1.3e-3 — indistinguishable, correctly, where raw cosine made
0.612 and 0.658 look different.

> **Limitation, stated rather than buried.** `noise_p` is *relative* to each query's own distribution.
> A far-off-topic query has a tight low distribution, so a modest top score can look extreme against
> it — `what is the capital of France` reports a more confident-looking `noise_p` than the near-domain
> react query. The gate therefore takes `max(absolute floor, fitted floor)`, and the absolute floor
> remains a required backstop. Fitting removes the *arbitrary* part of the decision, not the decision.
> The full normal–exponential mixture (below) models both components and would address this directly.

> **⚠️ Two decay curves over the same quantity (found 2026-07-30).** `recencyScore` (`rank.go`) decays
> `exp(−days/30)` and `retention` (`forget.go`) decays `exp(−days/halflife_days=90)` — both over
> `now − Updated`, both applied to the same decision in the same scoring pass, disagreeing by 3×, with
> nothing reconciling them. Worse, **both mislabel their constant**: `exp(−t/τ)` has half-life `τ·ln2`,
> so τ=90 gives a **62.4-day** half-life (not 90) and τ=30 gives **20.8 days** (not the "~30-day
> half-life-ish" the comment claims). Both knobs are off from their stated meaning by `ln2 ≈ 0.693`.
> Fix: one decay function, one correctly-named constant, fit per the reference above.

**Gate #0 is also not enforced.** §1.7 requires a build-time scanner that fails on literal constants;
none exists, and two literals survive in `rank.go` — the 30-day recency constant and the `spec/3.0`
importance divisor. The gate was declared non-negotiable and then enforced by intention.

## 12. Asking for feedback — active learning, run as a closed loop (`ask.go`) ✅

The feedback loop existed from Phase 0 and was almost entirely unused: **151 recalls produced 1
label.** That is not apathy, it is design — nothing ever prompted anyone, so labelling required
remembering the endpoint existed and choosing, unprompted, to use it. The cost is concrete and
blocking: the ~13 ranking constants that are still *chosen* rather than *fitted* (§11) can only be
fitted from interaction data — counterfactual / unbiased learning-to-rank (*Joachims, Swaminathan &
Schnabel, "Unbiased Learning-to-Rank with Biased Feedback", WSDM 2017*), which corrects for position
bias via propensity weighting — and there is nowhere near enough of it. **The blocker on those knobs
was never research. It was that the system never asked.**

**Which recalls to ask about — uncertainty sampling.** Not all of them: that is what makes feedback
prompts annoying enough to be switched off, and most labels would be worthless anyway (a recall at
`noise_p = 2e-15` is one the system is already certain about; confirming it teaches nothing). Query
the instances the model is *least certain* about, because those carry the most information per unit
of human patience — *Lewis & Gale (SIGIR 1994)*; *Settles, "Active Learning Literature Survey"
(2009)*. This system is unusually well set up for it: §11a made `noise_p` a calibrated probability,
so "least certain" is a **measured quantity** here, not a heuristic.

**How many to ask — a controller, not a schedule.** The ask rate is not a fixed cadence someone
guessed. The engine tracks, over a rolling window, what fraction of recalls came back **confident**,
and compares it to `feedback.target_confidence_ratio`. Above target, questions taper to a background
trickle; below it, they tighten in proportion to the shortfall, concentrating labelling effort
exactly when and where quality is slipping — and quieting down on their own once the ratio recovers.
A rolling window rather than a lifetime average, because the question is "how are we doing *now*": a
cumulative mean would let a long healthy history mask a recent regression for thousands of recalls.

**The blind spot, and why sampling is mixed in.** Uncertainty sampling never asks about cases the
model is confident on, so *on its own it can never discover CONFIDENT-AND-WRONG* — which is this
system's most dangerous failure mode, and a demonstrated one: the negation results come back
`weak:false`. A slice of confident recalls is therefore sampled too, deterministically (every Nth
recall, not an RNG, so the policy is reproducible in a test). Related: *Settles & Craven (EMNLP
2008)* weight uncertainty by density to avoid spending queries on outliers — the same instinct.

**Gaps in the corpus.** A near-miss abstention gets its own question — *"I found nothing, but
something came close. Should I have known it?"* Ranking feedback can only ever talk about decisions
that were surfaced, so a missing rule is invisible to it. This is the only channel through which the
corpus can learn what it does not contain.

Delivered through the recall hook that already injects context into Claude Code, so there is no new
surface to visit and no queue to remember. `feedback.ask_mode=off` silences it entirely — an
assistant that cannot be told to stop asking gets turned off completely instead.

**§10 — IR references.** These were cited inline in §10 but never listed here, which is part of why
§10a's normalization citation could sit next to code that ignored it without anyone noticing. Listed
now so every claim §10 makes has a row that can be checked against the implementation.

| # | Reference | Backs |
|---|---|---|
| Lee 1997 | Analyses of Multiple Evidence Combination (*SIGIR*) | §10a normalize **before** combining |
| Fox & Shaw 1994 | Combination of Multiple Searches (*TREC-2*) | §10a CombSUM / score normalization |
| Montague & Aslam 2001 | Relevance Score Normalization for Metasearch (*CIKM*) | §10a normalization |
| Wang, Lin & Metzler 2011 | A Cascade Ranking Model (*SIGIR*) | §10a relevance gates first |
| Kraaij, Westerveld & Hiemstra 2002 | Priors for entry page search (*SIGIR*) | §10a priors stay small |
| Cormack, Clarke & Buettcher 2009 | Reciprocal Rank Fusion (*SIGIR*) | §10a RRF mode (scale-immune) |
| Bendersky, Croft & Diao 2011 | Quality-biased ranking (*WSDM*) | §10a quality priors |
| Manmatha, Rath & Feng 2001 | Modeling Score Distributions (*SIGIR*) | §10c weak = standing out |
| Cronen-Townsend, Zhou & Croft 2002 | Predicting Query Performance / Clarity (*SIGIR*) | §10c QPP gate (deferred) |
| Zhou & Croft 2007 | Query performance prediction / WIG (*SIGIR*) | §10c QPP gate (deferred) |
| Platt 1999 / Lin et al. 2007 | Probabilistic outputs / calibration | §10c cosine → probability |
| Thakur et al. 2021 | BEIR (*NeurIPS D&B*) | §10c lexical hybrid gate |
| Gollapudi et al. 2023 | Filtered-DiskANN (*WWW*) | §10b hard facets as predicates |
| Hearst 2006 | Design recommendations for faceted search (*CACM*) | §10b facets |
| Weller et al. 2024 | NevIR (*EACL*) / FollowIR | §10d negation blindness |
| García-Ferrero et al. 2023 | Negation in LMs (*EMNLP*) | §10d negation |
| Nogueira & Cho 2019 | Passage Re-ranking with BERT (arXiv:1901.04085) | §10d cross-encoder re-rank |
| Kamvar, Schlosser & Garcia-Molina 2003 | EigenTrust (*WWW*) | §10a seeded authority |
| Arampatzis & Kamps 2009 | A signal-to-noise approach to score normalization (*CIKM*) | §11 auto-calibrated floor |
| Arampatzis, Kamps & Robertson 2009 | Where to stop reading a ranked list (*SIGIR*) | §11 cutoff / `precision_floor` |
| Arampatzis & Robertson 2011 | Modeling score distributions in IR (*Information Retrieval*) | §11 normal–exponential mixture |
| Bahri et al. 2021 | Learning to Truncate Ranked Lists (arXiv:2102.12793) | §11 learned cutoff |
| Settles & Meeder 2016 | A Trainable Spaced Repetition Model / HLR (*ACL*) | §11 per-item half-life |
