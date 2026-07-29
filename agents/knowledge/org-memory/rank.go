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
//   - PRECISION FLOOR on relevance: only surface decisions whose semantic relevance clears a bar;
//     if none clear it, return NOTHING — an empty result is valid and correct (irrelevant context
//     measurably hurts an agent's output). The floor is a live Config knob.
//   - ORDER by a composite score (relevance + recency + scope-importance + feedback nudge), most
//     relevant FIRST — because position matters ("lost in the middle": models attend to the ends).
//
// Feedback is a relevance nudge only (helpful/used boost, not_relevant demotes) — never correctness.
func rankAndFilter(cands []scored, cfg *Config, chain ...string) []RecalledItem {
	floor := cfg.F("retrieve.precision_floor", 0.30, chain...)
	topK := cfg.I("retrieve.top_k", 3, chain...)
	wRel := cfg.F("rank.w_relevance", 1.0, chain...)
	wRec := cfg.F("rank.w_recency", 0.3, chain...)
	wImp := cfg.F("rank.w_importance", 0.3, chain...)
	boost := cfg.F("feedback.boost_per_helpful", 0.1, chain...)
	demote := cfg.F("feedback.demote_per_notrel", 0.1, chain...)

	now := time.Now()

	type row struct {
		item  RecalledItem
		score float64
	}

	var rows []row

	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue // quarantined / superseded are kept in the store but never surfaced
		}

		if c.sim < floor {
			continue // relevance floor → inject nothing when nothing is relevant enough
		}

		fb := boost*float64(c.st.Helpful+c.st.Used) - demote*float64(c.st.NotRelevant)
		score := wRel*c.sim + wRec*recencyScore(c.d.Updated, now) + wImp*importance(c.spec) + fb

		rows = append(rows, row{
			item:  RecalledItem{Decision: c.d, Score: round(score), Guidance: render(c.d)},
			score: score,
		})
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })

	out := make([]RecalledItem, 0, topK)
	for i := 0; i < len(rows) && i < topK; i++ {
		out = append(out, rows[i].item)
	}

	return out
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
