# org-memory as a brain: what we have, what we don't, what to add

A research-backed audit mapping org-memory against human memory neuroscience and cognitive science.
Every citation was verified against a primary/authoritative source (Nature, Science, Neuron, *Psych.
Review*, PubMed, AOM, arXiv). Status is *our analytical assessment* of the current system, not a claim
from the papers.

**One-line thesis.** org-memory today is a strong *fast semantic store with forgetting and abstention*.
To become brain-like it needs three things it lacks: **surprise-gated encoding** (learn what the model
doesn't already know), **consolidation** (dedup + synthesize principles), and **transactive routing**
(who-knows-what → route to a human when it abstains).

---

## Master map — HAVE / PARTIAL / MISSING

| Brain mechanism | Verified citation | org-memory status |
|---|---|---|
| Semantic memory (context-free rules) | Tulving 1972; Tulving 1985 (*Am. Psych.* 40:385) | ✅ **HAVE** — decisions are semantic rules |
| Episodic memory (the event that produced it) | Tulving 1972 | 🟡 PARTIAL — source (PR/incident) kept as a dead provenance link, not a queryable episode |
| Working memory / episodic buffer | Baddeley & Hitch 1974; Baddeley 2000 (*TiCS* 4:417) | ✅ HAVE (the injected context binds retrieved rules + task) / 🟡 no central-executive control |
| Fast one-shot encoding | McClelland, McNaughton & O'Reilly 1995 (*Psych. Rev.* 102:419) | ✅ HAVE — capture is instant, no retraining |
| Encoding specificity / context-cued retrieval | Tulving & Thomson 1973; Godden & Baddeley 1975 | 🟡 PARTIAL — similarity-cued (have), applicability-scope matching (partial) |
| Levels of processing (deep encoding) | Craik & Lockhart 1972 | 🟡 PARTIAL — we extract what+why (deep) but not rejected-alternatives/conditions |
| Pattern **completion** (partial cue → whole) | Marr 1971; Yassa & Stark 2011 (*TiNS* 34:515) | 🟡 PARTIAL — dense retrieval approximates it |
| Forgetting / retrievability decay | Bjork & Bjork 1992; Richards & Frankland 2017 | ✅ HAVE — `forget.go` (demote-not-delete) |
| Sleep replay / prioritized consolidation | Diekelmann & Born 2010 (*NRN* 11:114); Schaul 2015 | 🟡 PARTIAL — decay exists; no error-weighted offline replay loop |
| Metamemory / feeling-of-knowing → abstention | Nelson & Narens 1990; Koriat 1993/1997 | 🟡 PARTIAL — we abstain, but on *cue-familiarity* (similarity), Koriat's *fallible* signal |
| Confidence calibration | Fleming & Lau 2014 (*Front. Hum. Neurosci.* 8:443) | 🟡 MISSING→PARTIAL — weak-flag is a score, not measured confidence↔accuracy (meta-d′) |
| Extended mind (external scaffold) | Clark & Chalmers 1998; Hutchins 1995 | ✅ HAVE (durable offload) / 🟡 untrusted (no calibration) |
| **Surprise-gated encoding (= necessity)** | **Schultz, Dayan & Montague 1997 (*Science* 275:1593)** | ❌ **MISSING** — captures everything equally, no model of the base LLM's default |
| **Predictive coding (store the residual)** | Rao & Ballard 1999; Friston 2010 (*NRN* 11:127) | ❌ MISSING — no predictor of default behavior to "explain away" the redundant |
| **Schemas / assimilation (synthesize principles)** | Bartlett 1932; Tse et al. 2007 (*Science* 316:76) | ❌ MISSING — flat list of atomic decisions; no upward synthesis |
| **Pattern separation / dedup** | Marr 1971; Yassa & Stark 2011 | ❌ MISSING — near-duplicates accumulate (58 "timezone" decisions) |
| **Reconsolidation (update-on-recall)** | Nader, Schafe & LeDoux 2000 (*Nature* 406:722) | ❌ MISSING — retrieval is read-only; only manual supersede |
| **Spreading activation / decision graph** | Collins & Loftus 1975; Anderson (ACT-R) | ❌ MISSING — flat vector, one hop, no association graph |
| **Transactive memory (who-knows-what)** | Wegner 1986; Lewis 2003 (*JAP* 88:587) | 🟡 PARTIAL — author/authority tags = embryonic *Specialization*; **Credibility + Coordination MISSING** |
| Organizational-memory bins | Walsh & Ungson 1991 (*AMR* 16:57) | 🟡 PARTIAL — strong on Individuals/archive; thin on Transformations; absent on Culture |

---

## The ADDs — six highest-leverage, each paper-backed

**1. Surprise-gated encoding — the self-learning core (Schultz 1997; Rao & Ballard 1999; Friston 2010).**
Before storing/boosting a decision, ask: *does the base model's default already comply?* Default-wrong =
high prediction error = high-value → encode & surface prominently. Default-right = no error → redundant →
down-weight/drop. This is dopamine's RPE and predictive coding's residual, and it is **identifiable** (a
measured behavioral gap), so it escapes the paper's impossibility result. *Being measured now by the v2
necessity probe; the next step is to store the necessity score per decision and weight ranking by it.*

**2. Pattern-separation dedup (Marr 1971; Yassa & Stark 2011).** Cluster the store in embedding space;
merge near-duplicates into one canonical decision (the 58 timezone entries → 1), keep genuinely distinct
edge-cases separate. Directly fixes the redundancy the red-team flagged and sharpens retrieval precision.

**3. Reconsolidation — update-on-use (Nader et al. 2000).** When a decision is recalled *and used*, treat
it as reactivation: fold the new context back in (refine wording, merge a corollary, tighten scope) and
re-store — instead of a static record that can only be superseded. Memory improves through use.

**4. Schema / reflection layer (Tse et al. 2007; CLS; Generative Agents reflection).** An offline pass
that synthesizes higher-level principles from decision clusters ("across N decisions, we route reads
through the aggregator, never the DB"). New decisions assimilate cheaply into the schema; the schema
becomes the fast-consolidating, high-priority retrieval target. This is the "slow store" of CLS we lack.

**5. Transactive routing — route to a human on abstain (Wegner 1986; Lewis 2003).** An org-memory *is* a
transactive system: a who-knows-what directory. Add **Credibility** (reliability-weight each source, not
just label it) and **Coordination** (when the memory abstains, *forward the query to the tagged expert*
instead of returning silence). Turns a passive archive into an active group mind.

**6. Calibrated confidence / abstention (Nelson & Narens 1990; Koriat 1997; Fleming & Lau 2014).** Our
abstention floor and weak-flag are *cue-familiarity* (similarity) — Koriat's fallible heuristic. Validate
them against ground truth (measure false-abstain / false-surface, meta-d′) and wrap surfaced decisions in
a conformal coverage guarantee, so "I have nothing" and "here, with 90% confidence" are *earned*.

---

## Two syntheses that tie it together

**A. The self-learning loop is the brain's surprise mechanism.** "Necessity" (does the model's default
already comply?) unifies three findings: it is the **encoding gate** (dopamine RPE — Schultz 1997), the
**architecture** that produces it (predictive-coding residual — Rao & Ballard 1999; Friston 2010), and the
**replay priority** for consolidation (prioritized experience replay — Schaul 2015). A memory built on
necessity is a predictive-coding memory: it stores its own errors against the base model and explains away
everything the model already gets right. *This is exactly the "uplift the junior" thesis: the memory's job
is the residual between what a junior defaults to and what the org decided.*

**B. org-memory is a transactive organizational memory, not an archive.** Composing Wegner 1986 (group
mind = directory + storage), Walsh & Ungson 1991 (retention bins), and Clark & Chalmers 1998 / Hutchins
1995 (extended mind / distributed cognition): the valuable object is not the decision text but the
**human+memory loop** — monitor coverage, abstain when empty, and **route to the right person**. What
graduates the system from archive to group mind is credibility-weighting + routing, both currently missing.

---

## References (all verified against primary/authoritative sources)

- Tulving 1972, *Organization of Memory* (Academic Press) 381–403; Tulving 1985, *Am. Psychologist* 40(4):385–398.
- Baddeley & Hitch 1974, *Psych. of Learning & Motivation* 8:47–89; Baddeley 2000, *TiCS* 4(11):417–423.
- McClelland, McNaughton & O'Reilly 1995, *Psych. Review* 102(3):419–457; Kumaran, Hassabis & McClelland 2016, *TiCS* 20(7):512–534.
- Tulving & Thomson 1973, *Psych. Review* 80(5):352–373; Godden & Baddeley 1975, *Br. J. Psychology* 66(3):325–331.
- Collins & Loftus 1975, *Psych. Review* 82(6):407–428; Anderson 1983, *JVLVB* 22(3):261–295 (ACT-R).
- Craik & Lockhart 1972, *JVLVB* 11(6):671–684.
- Nader, Schafe & LeDoux 2000, *Nature* 406(6797):722–726.
- Bartlett 1932, *Remembering* (Cambridge UP); Tse et al. 2007, *Science* 316(5821):76–82; Ghosh & Gilboa 2014, *Neuropsychologia*.
- Marr 1971, *Phil. Trans. R. Soc. B* 262(841):23–81; Yassa & Stark 2011, *Trends Neurosci.* 34(10):515–525.
- Schultz, Dayan & Montague 1997, *Science* 275(5306):1593–1599.
- Rao & Ballard 1999, *Nature Neuroscience* 2(1):79–87; Friston 2010, *Nature Rev. Neurosci.* 11(2):127–138.
- Diekelmann & Born 2010, *Nature Rev. Neurosci.* 11(2):114–126; Schaul et al. 2015, arXiv:1511.05952.
- Nelson & Narens 1990, *Psych. of Learning & Motivation* 26:125–173; Koriat 1993, *Psych. Review* 100(4):609–639; Koriat 1997, *JEP:General* 126(4):349–370.
- Fleming & Lau 2014, *Frontiers in Human Neuroscience* 8:443.
- Wegner 1986, in *Theories of Group Behavior* (Springer) 185–205; Lewis 2003, *J. Applied Psychology* 88(4):587–604.
- Walsh & Ungson 1991, *Academy of Management Review* 16(1):57–91.
- Hutchins 1995, *Cognition in the Wild* (MIT Press); Clark & Chalmers 1998, *Analysis* 58(1):7–19.

*Analytical caveats: HAVE/PARTIAL/MISSING are our design assessment, not empirical results from the cited
works. Bartlett 1932 and the Wegner 1985 companion are canonical sources not re-fetched; Clark & Chalmers
and Ghosh & Gilboa exact pages are the standard-cited values.*
