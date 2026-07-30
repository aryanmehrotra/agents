package main

import (
	"math"
	"sort"
	"time"
)

// retention estimates a decision's current RETRIEVABILITY in [0,1] — how accessible it should be
// right now — which is deliberately distinct from its permanent storage.
//
// Foundation: Bjork & Bjork 1992, "A New Theory of Disuse" — a memory has two independent strengths:
// storage strength (durable, only ever grows) and retrieval strength (fluctuates with use and
// disuse). "Forgetting" is low retrieval strength, not erasure. So we NEVER delete: a decision that
// decays is merely demoted out of the hot recall set (Richards & Frankland 2017 — adaptive forgetting
// improves memory-guided decisions by shedding stale detail), and it revives on its own the moment it
// proves useful again. Reinforcement/penalty come from the existing feedback counters; recency comes
// from Updated (bumped on capture/reinforce). Every constant here is a live Config knob (Gate #0).
func retention(d Decision, st Stats, now time.Time, cfg *Config, chain ...string) float64 {
	half := cfg.F("forget.halflife_days", 90, chain...)
	wUse := cfg.F("forget.reinforce_per_use", 0.5, chain...)
	wPen := cfg.F("forget.penalty_per_negative", 0.5, chain...)

	// freshness — exponential decay by age since last touch; in (0,1]. A zero/unknown timestamp is
	// treated as fully fresh: we only forget what we KNOW is old, never something with missing age.
	fresh := 1.0
	if !d.Updated.IsZero() {
		fresh = halfLifeDecay(ageDays(d.Updated, now), half)
	}

	// reinforcement and penalty — saturating, so a few positive signals lastingly protect a memory
	// (value-directed remembering; synaptic tagging) while negatives erode retrievability.
	use := float64(st.Helpful*2 + st.Used)

	// Only WRONG erodes retrievability — `not_relevant` deliberately does not.
	//
	// The two signals answer different questions, and conflating them lets a retrieval miss destroy a
	// true decision. `wrong` is a claim about the DECISION: it says the rule is false or outdated, and
	// a false rule should indeed fade out of memory. `not_relevant` is a claim about a query-document
	// PAIR: it says "this was a poor answer to what I asked", which is a statement about the match,
	// not about the decision's truth.
	//
	// That distinction is foundational, not a nicety: relevance is defined in the IR literature as a
	// RELATION between a document and an information need, never a property the document possesses on
	// its own (Saracevic, "Relevance: A review of the literature and a framework", JASIST 2007 — the
	// five manifestations are all relations; Mizzaro's history says the same). Recommender work draws
	// the same line between item quality and contextual relevance. Storing a relation as a property is
	// a category error, and here it was a destructive one: one bad match for one phrasing used to
	// penalise a correct decision's retrievability for EVERY future query, decaying it out of the
	// corpus entirely.
	pen := float64(st.Wrong * 2)
	reinforce := wUse * (1 - math.Exp(-use))
	penalty := wPen * (1 - math.Exp(-pen))

	r := fresh + reinforce - penalty

	return clamp01(r)
}

// halfLifeDecay is THE decay curve for this system — one function, so a "half-life" means the same
// thing everywhere it appears.
//
// It also fixes a units bug. The retention and recency curves were both written as `exp(-t/k)` with
// k read from a knob named for a half-life. But `exp(-t/τ)` has half-life `τ·ln2`, not τ — so
// `forget.halflife_days = 90` actually produced a 62.4-day half-life, and the recency curve's
// "≈30-day half-life-ish" was 20.8 days. Every such knob was off from its stated meaning by
// ln2 ≈ 0.693, which makes any attempt to FIT the value (see RESEARCH.md §11) fit the wrong quantity.
// Multiplying by ln2 here makes the knob mean exactly what it is named.
//
// Form: R = e^(−t·ln2/H), the standard retention curve (Ebbinghaus; Bjork & Bjork 1992 storage vs
// retrieval strength). A single global H is the naive version — Settles & Meeder (ACL 2016) regress
// half-life per item from observed recall, which is the documented upgrade path.
func halfLifeDecay(days, halflife float64) float64 {
	if halflife <= 0 || days < 0 {
		return 0
	}

	return math.Exp(-days * math.Ln2 / halflife)
}

// ageDays is the non-negative age of t in days (clock skew / future timestamps read as age 0).
func ageDays(t, now time.Time) float64 {
	d := now.Sub(t).Hours() / 24
	if d < 0 {
		return 0
	}

	return d
}

func clamp01(f float64) float64 {
	switch {
	case math.IsNaN(f):
		// NaN fails every comparison, so a naive clamp returns it unchanged and it propagates into
		// scores — where encoding/json then refuses to marshal it and the whole recall 500s. A knob
		// set to NaN (authority.<role> and role.<login> are unknown keys to validateConfig, so they
		// are not range-checked) reached exactly this path.
		return 0
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// ColdItem is one decision the consolidation sweep would let fade — reported, never deleted.
type ColdItem struct {
	ID        string  `json:"id"`
	What      string  `json:"what"`
	Retention float64 `json:"retention"`
	AgeDays   float64 `json:"age_days"`
	Helpful   int     `json:"helpful"`
	Reason    string  `json:"reason"`
}

// ConsolidationReport is the memory-health snapshot: how much of the store is still retrievable (hot)
// vs decayed below the forget floor (cold), and the coldest items with why. It is READ-ONLY — it
// reports what runtime retrievability is already doing; it deletes nothing.
type ConsolidationReport struct {
	Total     int        `json:"total"`
	Hot       int        `json:"hot"`
	Cold      int        `json:"cold"`
	ColdPct   float64    `json:"cold_pct"`
	Floor     float64    `json:"forget_floor"`
	HalflifeD float64    `json:"halflife_days"`
	Coldest   []ColdItem `json:"coldest"`
}

// Consolidate computes each active decision's retention and splits hot vs cold at the forget floor —
// the same floor recall uses to demote. Pure introspection: no writes, no deletes. It makes the
// otherwise-invisible forgetting legible ("here's what's fading and why"), so a human can reinforce
// anything worth keeping (which raises its retrievability) before it drops out of default recall.
func (en *Engine) Consolidate(now time.Time, limit int) ConsolidationReport {
	floor := en.cfg.F("forget.floor", 0.03)
	half := en.cfg.F("forget.halflife_days", 90)

	rep := ConsolidationReport{Floor: floor, HalflifeD: half}

	var cold []ColdItem

	for _, d := range en.store.Active() {
		if d.Quarantined {
			continue
		}

		st := en.store.Stats(d.ID)
		r := retention(d, st, now, en.cfg)
		rep.Total++

		if r >= floor {
			rep.Hot++
			continue
		}

		rep.Cold++

		ageDays := now.Sub(d.Updated).Hours() / 24
		cold = append(cold, ColdItem{
			ID: d.ID, What: d.What, Retention: round(r), AgeDays: math.Round(ageDays),
			Helpful: st.Helpful, Reason: coldReason(ageDays, st),
		})
	}

	if rep.Total > 0 {
		rep.ColdPct = math.Round(float64(rep.Cold)/float64(rep.Total)*1000) / 10
	}

	sort.Slice(cold, func(i, j int) bool { return cold[i].Retention < cold[j].Retention })

	if limit > 0 && len(cold) > limit {
		cold = cold[:limit]
	}

	rep.Coldest = cold

	return rep
}

// coldReason renders a short human explanation of why a decision is fading.
func coldReason(ageDays float64, st Stats) string {
	switch {
	case st.Wrong > 0 || st.NotRelevant > 0:
		return "negative feedback eroded retrievability"
	case st.Helpful == 0 && st.Used == 0:
		return "aged out, never reinforced (no helpful/used signal)"
	default:
		return "aged past its half-life"
	}
}
