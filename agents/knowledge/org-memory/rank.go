package main

import (
	"math"
	"sort"
	"strings"
	"time"
)

// scored pairs a candidate decision with the signals ranking needs.
type scored struct {
	d    Decision
	sim  float64 // cosine relevance to the query
	lex  float64 // BM25 lexical relevance (0 when the hybrid is disabled)
	spec int     // scope specificity (from scopeMatch)
	st   Stats   // feedback counters
}

// hybridRelevance fuses the dense and lexical signals into the single relevance number the rest of
// the ranker consumes, and returns it on the cosine scale so the floor, the fitted noise model and
// every reported similarity keep meaning the same thing.
//
// Fusion is by RANK (Reciprocal Rank Fusion, Cormack/Clarke/Buettcher SIGIR 2009), because a cosine
// and a BM25 score share no units — a value-space blend needs a normalisation constant that is
// corpus-specific and silently lets one signal dominate when the corpus shifts. RRF has no such
// constant. The fused rank order is then mapped back onto the ORIGINAL cosine values, so a document
// promoted by lexical evidence inherits the cosine of the position it earned. This keeps a single
// relevance scale for the gate rather than introducing a second one nothing else understands.
func hybridRelevance(cands []scored, cfg *Config, chain ...string) {
	w := cfg.F("rank.w_lexical", 0, chain...)
	if w <= 0 || len(cands) < 2 {
		return // hybrid disabled (default) — dense-only, byte-identical to before
	}

	kRRF := cfg.F("rank.rrf_k", 60, chain...)

	// Quarantined and superseded decisions are candidates at this point (the surfaceable filter runs
	// later, in rankAndFilter), and their cosines would otherwise enter the value pool below — letting
	// a retired-as-wrong decision donate its relevance mass to a live one and lift it over the floor.
	// Fuse only over decisions that could actually be surfaced.
	live := make([]int, 0, len(cands))

	for i := range cands {
		// A NaN score must never enter the remap: rank order puts it arbitrarily, and it would then
		// be ASSIGNED a real cosine from the pool while an honest document inherits the NaN and gets
		// gated. The D6 guard downstream would reject the wrong document.
		if math.IsNaN(cands[i].sim) || math.IsNaN(cands[i].lex) {
			continue
		}

		if !cands[i].d.Quarantined && cands[i].d.SupersededBy == "" {
			live = append(live, i)
		}
	}

	if len(live) < 2 {
		return
	}

	// RANKS, not just the value pool, must come from live candidates only. Excluding them from the
	// remap alone was not enough: a quarantined decision still occupied a RANK POSITION, which shifts
	// every live document's RRF score unevenly and can flip which live document wins. A
	// retired-as-wrong decision must not decide the order of the ones that replaced it.
	denseRank := rankIndex(len(live), func(k int) float64 { return cands[live[k]].sim })
	lexRank := rankIndex(len(live), func(k int) float64 { return cands[live[k]].lex })

	fused := make([]float64, len(live))
	for k := range live {
		fused[k] = 1/(kRRF+float64(denseRank[k]+1)) + w/(kRRF+float64(lexRank[k]+1))
	}

	// Sorted cosines of the live candidates: the value pool the fused order is mapped onto.
	sims := make([]float64, 0, len(live))
	for _, i := range live {
		sims = append(sims, cands[i].sim)
	}

	sort.Sort(sort.Reverse(sort.Float64Slice(sims)))

	for k, pos := range rankIndex(len(live), func(k int) float64 { return fused[k] }) {
		cands[live[k]].sim = sims[pos]
	}
}

// rankAndFilter turns candidates into the few items worth surfacing. Two proven rules:
//   - RELEVANCE GATE: only surface decisions whose semantic relevance clears a bar; if none clear
//     it, return NOTHING — an empty result is valid and correct (irrelevant context measurably hurts
//     an agent's output). The bar is the MAX of two things: a fixed absolute floor (a safety
//     backstop), and an ADAPTIVE floor that self-calibrates to the corpus — see gateFloor. The
//     adaptive part is why nobody has to hand-pick a magic cosine number per deployment.
//   - ORDER by a composite score (relevance + recency + scope-importance + feedback nudge), most
//     relevant FIRST — because position matters ("lost in the middle": models attend to the ends).
//
// Feedback is a relevance nudge only (helpful/used boost, not_relevant demotes) — never correctness.
//
// It also returns the EFFECTIVE floor it applied (absolute or adaptive, whichever bound). Callers need
// that number to describe an abstention honestly: "nothing cleared 0.65" is a measurement, whereas the
// bare empty list invites the caller to invent a similarity of 0.
func rankAndFilter(cands []scored, cfg *Config, chain ...string) ([]RecalledItem, float64, noiseModel) {
	// Clamp topK at the USE site (defense-in-depth): an unbounded/negative top_k (e.g. a hostile or
	// fat-fingered config write) previously flowed into make([]RecalledItem, 0, topK) and OOM-crashed
	// the process, and a huge value turned recall into a whole-store dump. Bound it regardless of config.
	topK := cfg.I("retrieve.top_k", 3, chain...)
	if topK < 1 {
		topK = 1
	}

	if maxK := cfg.I("retrieve.max_top_k", 50, chain...); topK > maxK {
		topK = maxK
	}

	wRel := cfg.F("rank.w_relevance", 1.0, chain...)
	wRec := cfg.F("rank.w_recency", 0.3, chain...)
	wImp := cfg.F("rank.w_importance", 0.3, chain...)
	wRet := cfg.F("rank.w_retention", 0.3, chain...)
	wAuth := cfg.F("rank.w_authority", 0.3, chain...)
	wFb := cfg.F("rank.w_feedback", 0.3, chain...)
	boost := cfg.F("feedback.boost_per_helpful", 0.1, chain...)
	demote := cfg.F("feedback.demote_per_notrel", 0.1, chain...)

	floor, noise := gateFloorWithNoise(cands, cfg, chain...)
	if floor < 0 { // a negative precision_floor would pass everything (a store-dump vector) — never allow it
		floor = 0
	}

	forgetFloor := cfg.F("forget.floor", 0.03, chain...)

	// Relevance-primary ranking with a BOUNDED prior tie-breaker. Cosine orders results; the weaker
	// priors (authority, recency, importance, retention, feedback) can only nudge the order by at most
	// `prior_cap` (≈0.01), so they break genuine near-ties WITHOUT ever burying a materially-more-
	// relevant answer. This is the lesson the red-team drove home: a fixed cosine-band still let priors
	// discard cosine *within* a band (recreating the burial and adding recency inversions); the robust
	// form is to keep cosine primary everywhere and hold the total positive prior below the smallest
	// cosine gap worth respecting. The prior is a weighted MEAN of [0,1] signals scaled by prior_cap —
	// bounded by construction, not by clamping a sum (see the loop below for why that distinction is
	// the whole ballgame). Negative feedback is NOT bounded — a "not relevant" mark should still demote
	// a wrong hit. Foundations: cascade ranking keeps relevance the gate (Wang/Lin/Metzler, SIGIR 2011);
	// priors belong as small terms, not large additive offsets (Kraaij et al., SIGIR 2002); and
	// heterogeneous signals must be normalized before combination (Lee SIGIR 1997).
	priorCap := cfg.F("rank.prior_cap", 0.01, chain...)

	now := time.Now()

	type row struct {
		item  RecalledItem
		sim   float64 // raw cosine (relevance list key)
		prior float64 // blended prior (prior list key)
		score float64 // capped-mode composite
	}

	var rows []row

	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue // quarantined / superseded are kept in the store but never surfaced
		}

		// NaN fails every comparison, so `c.sim < floor` is FALSE for NaN and a NaN-scored candidate
		// would sail through a gate whose entire claim is "bounded by construction", then sort
		// arbitrarily. Unreachable today (cosine guards zero norms), but a file that promises bounds
		// should not depend on a distant function's invariant to keep them.
		if math.IsNaN(c.sim) || c.sim < floor {
			continue // relevance gate → inject nothing when nothing is relevant enough
		}

		// Retrievability gate (forgetting): a decision decayed below the forget floor is demoted out
		// of the hot recall set — kept in the store, never deleted, and revives if reinforced.
		ret := retention(c.d, c.st, now, cfg, chain...)
		if ret < forgetFloor {
			continue
		}

		// BOUNDED PRIOR — normalize, THEN bound (never clamp a raw sum).
		//
		// The intent has always been "cosine decides; the priors only break genuine near-ties". The
		// previous form did not implement that intent: it summed the weighted priors and clamped the
		// SUM at prior_cap. Recency + importance + retention alone always exceed a 0.01 cap, so the sum
		// SATURATED for every candidate and each one received exactly +prior_cap. A constant offset
		// cannot reorder anything, so authority/recency/retention were inert — measurably: score−sim was
		// +0.010 for every item in every provenance, a CTO-authored review and an unattributed book
		// alike. The eviction-guard and w_authority-toggle checks "passed" vacuously, because a uniform
		// offset makes them unfalsifiable.
		//
		// The fix is the standard one for combining heterogeneous signals: NORMALIZE each signal onto a
		// common scale before the linear combination, instead of letting an unnormalized sum run away
		// and get clipped (Lee, "Analyses of Multiple Evidence Combination", SIGIR 1997; Montague &
		// Aslam, "Relevance Score Normalization for Metasearch", CIKM 2001). Every component here is
		// already defined on [0,1] — recency, importance, retention, authority.<role>, and a saturating
		// feedback term — so their weighted MEAN is in [0,1], and prior_cap × that mean is a prior that
		// is strictly bounded by prior_cap AND still carries ordering information. Relevance stays the
		// primary key (cascade ranking, Wang/Lin/Metzler SIGIR 2011) and the priors stay small
		// (Kraaij et al., SIGIR 2002) — the invariant the cap was there to protect is now enforced by
		// construction rather than by a clamp that destroyed the signal it was meant to bound.
		pos, wsum := 0.0, 0.0

		addPrior := func(w, v float64) {
			if w <= 0 {
				return // a zeroed weight drops the signal AND its share of the mean (a real toggle)
			}

			pos += w * clamp01(v)
			wsum += w
		}

		addPrior(wRec, recencyScore(c.d.Updated, now, cfg, chain...))
		addPrior(wImp, importance(c.spec, cfg, chain...))
		addPrior(wRet, ret)
		addPrior(wAuth, authorityWeight(c.d.Scope, cfg, chain...))
		// Positive feedback saturates (1−e^−x, as in retention): a few helpful/used signals count,
		// the hundredth adds almost nothing — so a popular decision can never buy its way up the list.
		addPrior(wFb, 1-math.Exp(-boost*float64(c.st.Helpful+c.st.Used)))

		priorNorm := 0.0
		if wsum > 0 {
			priorNorm = pos / wsum // weighted mean of [0,1] signals ⇒ itself in [0,1]
		}

		// Negative feedback stays OUTSIDE the bound, deliberately: a "not relevant" mark must be able to
		// push a wrong hit below its cosine neighbours, which a ≤prior_cap nudge could never do.
		demotion := demote * float64(c.st.NotRelevant)

		score := wRel*c.sim + priorCap*priorNorm - demotion

		rows = append(rows, row{
			item: RecalledItem{
				Decision: c.d, Score: round(score), Similarity: round(c.sim),
				Guidance: render(c.d, cfg, chain...),
			},
			sim:   c.sim,
			prior: priorNorm - demotion, // ordering key for the RRF prior list (demotion kept visible)
			score: score,
		})
	}

	// Fusion mode. "capped" (default) = relevance-primary with a bounded prior tie-breaker.
	// "rrf" = Reciprocal Rank Fusion (Cormack, Clarke & Buettcher, SIGIR 2009): fuse independent ranked
	// lists (relevance, prior) by Σ wᵢ/(k+rankᵢ). Rank-based ⇒ scale-immune (no score-magnitude can
	// dominate), but it also discards cosine MAGNITUDE, so we weight the relevance list heavily and keep
	// it selectable rather than forced — the two are compared empirically before either is made default.
	if strings.EqualFold(strings.TrimSpace(cfg.Str("rank.mode", "capped", chain...)), "rrf") {
		k := cfg.F("rank.rrf_k", 60, chain...)
		rw := cfg.F("rank.rrf_w_relevance", 1.0, chain...)
		pw := cfg.F("rank.rrf_w_prior", 0.2, chain...)

		relRank := rankIndex(len(rows), func(i int) float64 { return rows[i].sim })
		priRank := rankIndex(len(rows), func(i int) float64 { return rows[i].prior })

		for i := range rows {
			rows[i].score = rw/(k+float64(relRank[i]+1)) + pw/(k+float64(priRank[i]+1))
			rows[i].item.Score = round(rows[i].score)
		}
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })

	out := make([]RecalledItem, 0, topK)
	for i := 0; i < len(rows) && i < topK; i++ {
		out = append(out, rows[i].item)
	}

	return out, floor, noise
}

// gateFloor computes the relevance bar a candidate must clear. It is the MAX of:
//   - an absolute floor (retrieve.precision_floor) — a fixed safety backstop, and
//   - an ADAPTIVE floor: mean + z·stddev of THIS query's similarity distribution over the corpus.
//
// The adaptive term is the whole point of "auto-adapt": an absolute cosine threshold is
// corpus-specific (0.60 fits one tight repo, nonsense for another). "Is this a standout for THIS
// query, relative to the corpus's own spread?" transfers across corpora with no hand-tuning. For an
// irrelevant prompt the scores are flat (no standout ⇒ nothing clears mean+z·σ ⇒ inject nothing);
// for a real hit a few decisions spike into the right tail and clear it. z (retrieve.adaptive_z,
// default 2.0) is a near-universal knob; set it ≤ 0 to disable adaptation and use the absolute floor
// alone. Needs a minimum sample (retrieve.adaptive_min_n, default 8) for σ to mean anything.
func gateFloor(cands []scored, cfg *Config, chain ...string) float64 {
	floor, _ := gateFloorWithNoise(cands, cfg, chain...)
	return floor
}

// gateFloorWithNoise is gateFloor plus the fitted noise model, so callers that need to describe their
// confidence (recall's `weak` signal) use the SAME model that made the admit/reject decision rather
// than a second, unrelated heuristic. Those two disagreeing is how the old `weak` flag ended up
// firing on 16/32 correct answers.
func gateFloorWithNoise(cands []scored, cfg *Config, chain ...string) (float64, noiseModel) {
	floor := cfg.F("retrieve.precision_floor", 0.30, chain...)

	// The adaptive floor is expressed as a FALSE-INJECT BUDGET: "at most this fraction of irrelevant
	// decisions may clear the gate by chance." That is a quantity an operator can reason about and
	// that transfers across corpora and embedding models — unlike the sigma count it replaces, which
	// was set to 2.0 because 2 is a conventional number of sigmas. Set ≤0 to disable adaptation and
	// use the absolute floor alone. Foundations: score-distribution thresholding (Arampatzis, Kamps &
	// Robertson, SIGIR 2009); signal-to-noise normalisation (Arampatzis & Kamps, CIKM 2009).
	alpha := cfg.F("retrieve.max_false_inject_rate", 0, chain...)

	// A scale estimated from a handful of points is itself noise: the relative standard error of σ̂ is
	// ~1/√(2(n−1)) — 27% at n=8, 13% at n=30 — and MAD is only ~37% efficient at the normal, so it
	// wants MORE samples, not fewer. The old default of 8 let a 27%-uncertain scale set a threshold
	// and call it calibration.
	minN := cfg.I("retrieve.adaptive_min_n", 30, chain...)

	sims := make([]float64, 0, len(cands))

	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue // background = only surfaceable decisions
		}

		sims = append(sims, c.sim)
	}

	noise := fitNoiseModel(sims, minN)

	if alpha <= 0 || !noise.ok {
		return floor, noise
	}

	if adaptive, ok := noise.scoreAtTailProb(alpha); ok && adaptive > floor {
		return adaptive, noise
	}

	return floor, noise
}

// authorityWeight scores a decision by the SENIORITY/authority of its author — a CTO's judgment
// outranks a senior engineer's outranks a junior's when relevance is close. This is the
// provenance/authority-weighting invariant (cf. EigenTrust: trust weighted by an externally-seeded
// role, not self-asserted). Everything is Config (Gate #0 — nothing hardcoded): `role.<login>` maps a
// person to a role; `authority.<role>` is that role's weight; unknown people fall back to the default
// role/weight. A decision with no author tag (docs, seed) contributes no authority signal.
func authorityWeight(scope []string, cfg *Config, chain ...string) float64 {
	defRole := cfg.Str("authority.default_role", "sde2", chain...)
	defWeight := cfg.F("authority."+defRole, 0.3, chain...)

	for _, s := range scope {
		if !strings.HasPrefix(s, "author:") {
			continue
		}

		login := strings.TrimPrefix(s, "author:")
		role := cfg.Str("role."+login, defRole, chain...)

		return cfg.F("authority."+role, defWeight, chain...)
	}

	return 0
}

// rankIndex returns, for each item i, its 0-based position when the n items are ordered by key(i)
// descending — i.e. the highest key gets rank 0. Used to build the per-signal ranked lists that
// Reciprocal Rank Fusion (Cormack et al. 2009) fuses.
func rankIndex(n int, key func(int) float64) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a, b int) bool { return key(order[a]) > key(order[b]) })

	rank := make([]int, n)
	for pos, idx := range order {
		rank[idx] = pos
	}

	return rank
}

// recencyScore decays with age since last touch, in (0,1]. Zero time → 0.
//
// NOTE — this signal OVERLAPS `retention` (forget.go), which decays the same quantity with its own
// half-life. Both feed the prior, so freshness is counted twice, with two different weights and
// previously two different (and mislabelled) curves — 20.8 days here against 62.4 there, disagreeing
// by 3× on the same decision in the same scoring pass. They are now the same function with separate,
// correctly-named knobs, because they answer different questions: recency asks "is this rule current?"
// while retention asks "is this memory still retrievable?" (Bjork & Bjork's retrieval strength, which
// also folds in reinforcement). Keeping both is a deliberate choice; the double-count is real and is
// why `rank.w_recency` and `rank.w_retention` should not be tuned independently. See RESEARCH.md §11.
func recencyScore(t, now time.Time, cfg *Config, chain ...string) float64 {
	if t.IsZero() {
		return 0
	}

	return halfLifeDecay(ageDays(t, now), cfg.F("rank.recency_halflife_days", 21, chain...))
}

// importance rewards scope specificity — a repo/service rule matters more than a global one. The
// saturation point (how many scope tags count as "fully specific") is a knob, not a literal: what
// counts as specific is a property of the org's tagging convention, not of this engine.
func importance(spec int, cfg *Config, chain ...string) float64 {
	full := cfg.F("rank.importance_saturation", 3, chain...)
	if spec <= 0 || full <= 0 {
		return 0
	}

	return clamp01(float64(spec) / full)
}

// render is the advice line an agent injects — what, then the why, then the source.
//
// Everything here is attacker-controllable in an open deployment (`/capture` accepts what/why/source
// verbatim), and it lands directly in an agent's context, so rendering is a security boundary rather
// than formatting. Two rules, see trust.go for why:
//   - text is sanitised to a single line, so stored content cannot fabricate message structure;
//   - the `[src: …]` marker is an ATTESTATION and is only emitted for org-verified provenance —
//     otherwise the source is still shown, but explicitly marked unverified, because a marker the
//     content itself can forge is worse than no marker at all.
func render(d Decision, cfg *Config, chain ...string) string {
	maxWhat := cfg.I("advise.max_what_runes", 400, chain...)
	maxWhy := cfg.I("advise.max_why_runes", 600, chain...)
	maxSrc := cfg.I("advise.max_source_runes", 120, chain...)

	var b strings.Builder

	b.WriteString(sanitizeAdvice(d.What, maxWhat))

	if why := sanitizeAdvice(d.Why, maxWhy); why != "" {
		b.WriteString(" — " + why)
	}

	if src := sanitizeAdvice(d.Source, maxSrc); src != "" {
		if trustedProvenance(d.Provenance, cfg, chain...) {
			b.WriteString("  [src: " + src + "]")
		} else {
			b.WriteString("  [unverified src: " + src + "]")
		}
	}

	return b.String()
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }
