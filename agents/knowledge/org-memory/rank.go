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
	spec int     // scope specificity (from scopeMatch)
	st   Stats   // feedback counters
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
func rankAndFilter(cands []scored, cfg *Config, chain ...string) []RecalledItem {
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
	boost := cfg.F("feedback.boost_per_helpful", 0.1, chain...)
	demote := cfg.F("feedback.demote_per_notrel", 0.1, chain...)

	floor := gateFloor(cands, cfg, chain...)
	if floor < 0 { // a negative precision_floor would pass everything (a store-dump vector) — never allow it
		floor = 0
	}

	forgetFloor := cfg.F("forget.floor", 0.03, chain...)

	// Cascade ranking (Wang, Lin & Metzler, SIGIR 2011): relevance GATES recall; the weaker priors
	// (authority, recency, importance, retention, feedback) only re-rank WITHIN a relevance band, never
	// across one. band = ⌊cosine / bandWidth⌋; a decision in a higher cosine band always outranks a
	// lower-band one regardless of its author's seniority, so a bounded static prior can no longer evict
	// a materially-more-relevant answer (the additive-domination bug). Within a band, prior signals
	// break the near-tie. bandWidth (≈0.05) is a live knob.
	bandWidth := cfg.F("rank.band_width", 0.05, chain...)

	now := time.Now()

	type row struct {
		item  RecalledItem
		band  int
		score float64 // secondary (within-band) key: the prior blend
	}

	var rows []row

	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue // quarantined / superseded are kept in the store but never surfaced
		}

		if c.sim < floor {
			continue // relevance gate → inject nothing when nothing is relevant enough
		}

		// Retrievability gate (forgetting): a decision decayed below the forget floor is demoted out
		// of the hot recall set — kept in the store, never deleted, and revives if reinforced.
		ret := retention(c.d, c.st, now, cfg, chain...)
		if ret < forgetFloor {
			continue
		}

		fb := boost*float64(c.st.Helpful+c.st.Used) - demote*float64(c.st.NotRelevant)
		auth := wAuth * authorityWeight(c.d.Scope, cfg, chain...)
		// prior blend used ONLY within a band; cosine is intentionally excluded (the band already
		// captured relevance) so seniority/recency decide near-ties, not a residual cosine sliver.
		prior := wRec*recencyScore(c.d.Updated, now) + wImp*importance(c.spec) + wRet*ret + auth + fb
		display := wRel*c.sim + prior

		rows = append(rows, row{
			item:  RecalledItem{Decision: c.d, Score: round(display), Similarity: round(c.sim), Guidance: render(c.d)},
			band:  bandOf(c.sim, bandWidth),
			score: prior,
		})
	}

	// Higher cosine BAND always wins (relevance gate); ties within a band go to the prior blend.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].band != rows[j].band {
			return rows[i].band > rows[j].band
		}

		return rows[i].score > rows[j].score
	})

	out := make([]RecalledItem, 0, topK)
	for i := 0; i < len(rows) && i < topK; i++ {
		out = append(out, rows[i].item)
	}

	return out
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
	floor := cfg.F("retrieve.precision_floor", 0.30, chain...)
	z := cfg.F("retrieve.adaptive_z", 0, chain...)
	minN := cfg.I("retrieve.adaptive_min_n", 8, chain...)

	if z <= 0 {
		return floor
	}

	sims := make([]float64, 0, len(cands))
	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue // background = only surfaceable decisions
		}

		sims = append(sims, c.sim)
	}

	if len(sims) < minN {
		return floor // too few to estimate a spread; fall back to the absolute floor
	}

	mean, sd := meanStd(sims)
	if adaptive := mean + z*sd; adaptive > floor {
		return adaptive
	}

	return floor
}

// meanStd returns the mean and population standard deviation of xs (len(xs) > 0 assumed by caller).
func meanStd(xs []float64) (mean, std float64) {
	for _, x := range xs {
		mean += x
	}

	mean /= float64(len(xs))

	var v float64
	for _, x := range xs {
		d := x - mean
		v += d * d
	}

	return mean, math.Sqrt(v / float64(len(xs)))
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

// bandOf quantises a cosine similarity into a relevance band (Wang/Lin/Metzler cascade + standard
// bucketed tie-breaking). Items in the same band are treated as relevance-equivalent, so weaker
// priors decide only among them — never across bands. A non-positive width disables banding.
func bandOf(sim, width float64) int {
	if width <= 0 {
		return 0
	}

	return int(math.Floor(sim / width))
}

// recencyScore decays exponentially with age (~30-day half-life-ish), in (0,1]. Zero time → 0.
func recencyScore(t, now time.Time) float64 {
	if t.IsZero() {
		return 0
	}

	days := now.Sub(t).Hours() / 24
	if days < 0 {
		days = 0
	}

	return math.Exp(-days / 30.0)
}

// importance rewards scope specificity — a repo/service rule matters more than a global one.
func importance(spec int) float64 {
	switch {
	case spec <= 0:
		return 0
	case spec >= 3:
		return 1
	default:
		return float64(spec) / 3.0
	}
}

// render is the advice line an editor/agent injects — what, then the why, then the source.
func render(d Decision) string {
	var b strings.Builder

	b.WriteString(d.What)

	if d.Why != "" {
		b.WriteString(" — " + d.Why)
	}

	if d.Source != "" {
		b.WriteString("  [src: " + d.Source + "]")
	}

	return b.String()
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }
