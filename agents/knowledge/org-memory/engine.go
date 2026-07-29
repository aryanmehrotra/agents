package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	errEmptyWhat  = errors.New("decision `what` is required")
	errNoDecision = errors.New("feedback requires `decision_id`")
	errBadSignal  = errors.New("unknown feedback signal")
)

// Engine is the org-memory core: capture → store → recall → advise, plus the feedback loop. Every
// dependency is an interface (Store, Embedder, Config), so the engine is fully unit-testable with no
// infra and every backing is swappable. Phase 0 is advisory-only — it surfaces prior decisions as
// context and learns *relevance* from feedback; it never asserts correctness.
type Engine struct {
	store Store
	embed Embedder
	cfg   *Config
}

func NewEngine(s Store, e Embedder, c *Config) *Engine {
	return &Engine{store: s, embed: e, cfg: c}
}

// Capture records a new decision, or reinforces an existing identical one (dedupe by content hash so
// re-recording the same knowledge enriches it instead of duplicating). An optional `Supersedes`
// records an override — the old row is kept, never deleted.
func (en *Engine) Capture(ctx context.Context, in Decision) (Decision, error) {
	in.What = strings.TrimSpace(in.What)
	if in.What == "" {
		return Decision{}, errEmptyWhat
	}

	id := idFor(in.What, in.Scope)
	now := time.Now().UTC()

	if existing, ok := en.store.Get(id); ok && existing.SupersededBy == "" {
		existing.Why = firstNonEmpty(existing.Why, in.Why)
		existing.Source = firstNonEmpty(existing.Source, in.Source)
		existing.Provenance = firstNonEmpty(existing.Provenance, in.Provenance)
		existing.Updated = now
		en.store.Put(existing)

		return existing, nil
	}

	emb, err := en.embed.Embed(ctx, in.What+" "+in.Why)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		ID: id, What: in.What, Why: in.Why, Scope: in.Scope,
		Provenance: in.Provenance, Source: in.Source, Embedding: emb,
		Created: now, Updated: now,
	}

	if s := strings.TrimSpace(in.Supersedes); s != "" && en.store.Supersede(s, id) {
		d.Supersedes = s
	}

	en.store.Put(d)
	en.linkScopeEdges(d)

	return d, nil
}

// Recall returns the few prior decisions worth surfacing for a context — or nothing. Dense cosine
// over scope-matched, non-superseded candidates, then rankAndFilter (precision floor + ordering +
// feedback nudge). The chain (person/team scopes) lets config knobs resolve most-specific-first.
func (en *Engine) Recall(ctx context.Context, queryContext []string, chain ...string) ([]RecalledItem, error) {
	qvec, err := en.embed.Embed(ctx, strings.Join(queryContext, " "))
	if err != nil {
		return nil, err
	}

	cset := contextSet(queryContext)

	var cands []scored

	for _, d := range en.store.Active() {
		ok, spec := scopeMatch(d.Scope, cset)
		if !ok {
			continue
		}

		cands = append(cands, scored{
			d:    d,
			sim:  cosine(qvec, d.Embedding),
			spec: spec,
			st:   en.store.Stats(d.ID),
		})
	}

	return rankAndFilter(cands, en.cfg, chain...), nil
}

// RecordFeedback logs a reaction (append-only) and applies the simple Phase-0 effect: helpful/used
// boost, not_relevant demotes, and repeated wrong/outdated auto-quarantines (threshold is a live
// knob). This is a *relevance* signal only — never treated as correctness.
func (en *Engine) RecordFeedback(f Feedback) error {
	if strings.TrimSpace(f.DecisionID) == "" {
		return errNoDecision
	}

	switch f.Signal {
	case "helpful", "used", "not_relevant":
		en.store.RecordFeedback(f)
		en.store.Bump(f.DecisionID, f.Signal, 1)
	case "wrong", "outdated":
		en.store.RecordFeedback(f)
		en.store.Bump(f.DecisionID, f.Signal, 1)

		if en.store.Stats(f.DecisionID).Wrong >= en.cfg.I("feedback.wrong_quarantine_at", 2) {
			en.store.Quarantine(f.DecisionID)
		}
	default:
		return errBadSignal
	}

	return nil
}

// linkScopeEdges records associative edges to existing decisions that share a scope tag, for the
// Phase-2 graph-walk. Storing them now is cheap and needs no change to Phase-0 recall (dense+scope).
func (en *Engine) linkScopeEdges(d Decision) {
	tags := scopeTags(d.Scope)
	if len(tags) == 0 {
		return
	}

	for _, other := range en.store.Active() {
		if other.ID == d.ID {
			continue
		}

		for t := range scopeTags(other.Scope) {
			if tags[t] {
				en.store.LinkEdge(d.ID, other.ID, "scope:"+t, 1)

				break
			}
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
