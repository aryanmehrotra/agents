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
		ageDays := now.Sub(d.Updated).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}

		fresh = math.Exp(-ageDays / math.Max(1, half))
	}

	// reinforcement and penalty — saturating, so a few positive signals lastingly protect a memory
	// (value-directed remembering; synaptic tagging) while negatives erode retrievability.
	use := float64(st.Helpful*2 + st.Used)
	pen := float64(st.Wrong*2 + st.NotRelevant)
	reinforce := wUse * (1 - math.Exp(-use))
	penalty := wPen * (1 - math.Exp(-pen))

	r := fresh + reinforce - penalty

	return clamp01(r)
}

func clamp01(f float64) float64 {
	switch {
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
	ID          string  `json:"id"`
	What        string  `json:"what"`
	Retention   float64 `json:"retention"`
	AgeDays     float64 `json:"age_days"`
	Helpful     int     `json:"helpful"`
	Reason      string  `json:"reason"`
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
		rep.ColdPct = math.Round(float64(rep.Cold) / float64(rep.Total) * 1000) / 10
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
