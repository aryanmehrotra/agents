package main

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
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

	// live counters (since start) for the value metrics
	recalls      int64
	recallsEmpty int64
	surfaced     int64
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

	emb, err := en.embed.Embed(ctx, in.What+" "+in.Why, RoleDocument)
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

// defaultProbes are generic, deliberately OFF-TOPIC sentences used to measure a corpus's "noise
// ceiling": the highest semantic similarity that irrelevant text reaches against this specific
// memory. Real, relevant queries score above this ceiling; noise sits at or below it. They are
// domain-neutral on purpose — override via retrieve.calibration_probes for a corpus where some of
// these are actually on-topic (e.g. a travel or trivia knowledge base).
var defaultProbes = []string{
	"what is the capital of France",
	"write a poem about cats",
	"best pizza in New York",
	"how tall is Mount Everest",
	"who won the world cup",
	"recipe for chocolate chip cookies",
	"what time is sunset today",
	"lyrics to a pop song",
}

// Calibrate auto-sets retrieve.precision_floor from the corpus itself, so nobody hand-picks a cosine
// number. It embeds each off-topic probe, finds the max similarity any probe reaches against the
// store (the noise ceiling), and sets the floor a small margin above it. This adapts per corpus: a
// dense single-repo memory and a sprawling multi-team one get different, appropriate floors with no
// human tuning. Returns the floor it set (and the measured ceiling). No-op-safe on an empty store.
func (en *Engine) Calibrate(ctx context.Context, probes []string, margin float64) (floor, ceiling float64, err error) {
	if len(probes) == 0 {
		probes = defaultProbes
	}

	active := en.store.Active()
	if len(active) == 0 {
		return en.cfg.F("retrieve.precision_floor", 0.30), 0, nil // nothing to calibrate against
	}

	for _, p := range probes {
		qvec, e := en.embed.Embed(ctx, p, RoleQuery)
		if e != nil {
			return 0, 0, e
		}

		for _, d := range active {
			if s := cosine(qvec, d.Embedding); s > ceiling {
				ceiling = s
			}
		}
	}

	floor = ceiling + margin
	if floor > 0.99 {
		floor = 0.99
	}

	en.cfg.Set("retrieve.precision_floor", strconv.FormatFloat(floor, 'f', 4, 64))

	return floor, ceiling, nil
}

// Recall returns the few prior decisions worth surfacing for a context — or nothing. Dense cosine
// over scope-matched, non-superseded candidates, then rankAndFilter (precision floor + ordering +
// feedback nudge). The chain (person/team scopes) lets config knobs resolve most-specific-first.
func (en *Engine) Recall(ctx context.Context, queryContext []string, chain ...string) ([]RecalledItem, error) {
	// Separate scope TAGS (key:value — used for matching) from free TEXT (used for the embedding).
	// Mixing tags into the embedded text pollutes similarity: within one repo every query would look
	// alike (dominated by "repo:x"), so unrelated prompts would still match. Embed the text only.
	var textParts, tagParts []string

	for _, s := range queryContext {
		if isScopeTag(s) {
			tagParts = append(tagParts, s)
		} else if strings.TrimSpace(s) != "" {
			textParts = append(textParts, s)
		}
	}

	// No free text → nothing to match on semantically. Embedding an empty string yields a constant
	// vector that spuriously "matches" a fixed few decisions, so a blank/whitespace/scope-only query
	// must return NOTHING rather than canned advice.
	queryText := strings.TrimSpace(strings.Join(textParts, " "))
	if queryText == "" {
		atomic.AddInt64(&en.recalls, 1)
		atomic.AddInt64(&en.recallsEmpty, 1)

		return nil, nil
	}

	qvec, err := en.embed.Embed(ctx, queryText, RoleQuery)
	if err != nil {
		return nil, err
	}

	cset := contextSet(tagParts)
	en.expandAncestors(cset) // hierarchy inheritance: a child query also matches its ancestors' decisions

	// SOFT context (encoding-specificity, not a hard wall): the current context is a retrieval PRIOR,
	// not a filter. Every active decision is a candidate; a decision whose scope matches here gets a
	// specificity boost in ranking (spec>0 → importance term), while an out-of-context decision can
	// still surface if it's semantically relevant enough to clear the floor. This is why a decision
	// made in one repo can still surface in another when it genuinely applies — but same-repo
	// decisions are preferred. Hard scope-gating (the old behaviour) walled generalizable wisdom
	// inside the repo it happened to be recorded in. Set retrieve.hard_scope=1 to restore filtering.
	hardScope := en.cfg.F("retrieve.hard_scope", 0) > 0

	// HARD facets vs SOFT priors (Filtered-DiskANN, Gollapudi et al. 2023; Hearst 2006): an explicitly
	// asserted identity/provenance facet — author:… (WHO) or kind:… (what TYPE, e.g. kind:book) — is a
	// correctness PREDICATE: a wrong-author or wrong-kind result is simply incorrect, so it is excluded
	// from the candidate set, not merely down-weighted. Topical scope (repo:/service:/topic:) stays a
	// SOFT prior above. This is why "author=vikash" no longer leaks other authors and "scope=kind:book"
	// no longer returns non-book items.
	hardFacets := hardFilterTags(tagParts)

	var cands []scored

	for _, d := range en.store.Active() {
		if !hasAllTags(d.Scope, hardFacets) {
			continue // failed a hard facet predicate — cannot be correct for this request
		}

		ok, spec := scopeMatch(d.Scope, cset)
		if hardScope && !ok {
			continue
		}

		cands = append(cands, scored{
			d:    d,
			sim:  cosine(qvec, d.Embedding),
			spec: spec, // 0 when out of context → no context boost, but still eligible on relevance
			st:   en.store.Stats(d.ID),
		})
	}

	items := rankAndFilter(cands, en.cfg, chain...)

	atomic.AddInt64(&en.recalls, 1)
	atomic.AddInt64(&en.surfaced, int64(len(items)))

	if len(items) == 0 {
		atomic.AddInt64(&en.recallsEmpty, 1)
	}

	return items, nil
}

// ValueStats is the numerical value dashboard: usage, feedback, and an estimated token ROI. The two
// estimate rates are live Config knobs (metrics.tokens_per_item, metrics.tokens_per_prevented) so the
// numbers reflect your own assumptions; a rigorous lift needs A/B withholding (roadmap).
type ValueStats struct {
	Decisions        int     `json:"decisions"`
	Recalls          int64   `json:"recalls"`
	RecallsEmpty     int64   `json:"recalls_empty"`
	ItemsSurfaced    int64   `json:"items_surfaced"`
	Helpful          int     `json:"helpful"`
	Used             int     `json:"used"`
	NotRelevant      int     `json:"not_relevant"`
	Wrong            int     `json:"wrong"`
	PrecisionPct     float64 `json:"precision_pct"`
	InjectNothingPct float64 `json:"inject_nothing_pct"`
	Hot              int     `json:"hot"`      // decisions still retrievable (above the forget floor)
	Cold             int     `json:"cold"`     // decayed below the floor — demoted, not deleted
	ColdPct          float64 `json:"cold_pct"` // memory-health: share fading out of hot recall
}

// ValueStats computes the dashboard. Feedback tallies are read from the durable store; recall counters
// are since-start.
func (en *Engine) ValueStats() ValueStats {
	act := en.store.Active()
	now := time.Now()
	forgetFloor := en.cfg.F("forget.floor", 0.03)

	var h, u, nr, w, hot, cold int

	for _, d := range act {
		st := en.store.Stats(d.ID)
		h += st.Helpful
		u += st.Used
		nr += st.NotRelevant
		w += st.Wrong

		if retention(d, st, now, en.cfg) >= forgetFloor {
			hot++
		} else {
			cold++
		}
	}

	recalls := atomic.LoadInt64(&en.recalls)
	empty := atomic.LoadInt64(&en.recallsEmpty)
	surf := atomic.LoadInt64(&en.surfaced)

	prec := 0.0
	if h+w > 0 {
		prec = 100 * float64(h) / float64(h+w)
	}

	emptyPct := 0.0
	if recalls > 0 {
		emptyPct = 100 * float64(empty) / float64(recalls)
	}

	coldPct := 0.0
	if len(act) > 0 {
		coldPct = math.Round(float64(cold)/float64(len(act))*1000) / 10
	}

	return ValueStats{
		Decisions: len(act), Recalls: recalls, RecallsEmpty: empty, ItemsSurfaced: surf,
		Helpful: h, Used: u, NotRelevant: nr, Wrong: w,
		PrecisionPct: prec, InjectNothingPct: emptyPct,
		Hot: hot, Cold: cold, ColdPct: coldPct,
	}
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

// DecisionView is a decision plus its feedback stats — the read-only monitoring/brain shape.
type DecisionView struct {
	Decision Decision `json:"decision"`
	Stats    Stats    `json:"stats"`
}

// List returns all active (non-superseded) decisions with their feedback stats. Read-only — it changes
// no behaviour; it exists so a dashboard can visualise the whole memory ("the brain").
func (en *Engine) List() []DecisionView {
	act := en.store.Active()
	out := make([]DecisionView, 0, len(act))

	for _, d := range act {
		out = append(out, DecisionView{Decision: d, Stats: en.store.Stats(d.ID)})
	}

	return out
}

// --- scope hierarchy (parent → child relations, confirmed via the API / yes-no) ---

// HierarchyView is the read shape for the UI: all relations + the scope tags currently in use.
type HierarchyView struct {
	Relations []Relation `json:"relations"`
	Tags      []string   `json:"tags"` // distinct scope tags across active decisions
}

// Hierarchy returns the stored relations plus the scope tags in use, so the UI can render the tree.
func (en *Engine) Hierarchy() HierarchyView {
	tagset := map[string]bool{}

	for _, d := range en.store.Active() {
		for _, s := range d.Scope {
			if t := strings.TrimSpace(s); t != "" {
				tagset[t] = true
			}
		}
	}

	tags := make([]string, 0, len(tagset))
	for t := range tagset {
		tags = append(tags, t)
	}

	sort.Strings(tags)

	return HierarchyView{Relations: en.store.Relations(), Tags: tags}
}

// SetParent confirms a parent→child relation (the user setting the tree).
func (en *Engine) SetParent(child, parent string) {
	en.store.SetRelation(strings.TrimSpace(child), strings.TrimSpace(parent), "confirmed")
}

// ConfirmRelation records a yes/no on a proposed relation: accept → confirmed, reject → rejected
// (kept so it is not proposed again).
func (en *Engine) ConfirmRelation(child, parent string, accept bool) {
	status := "rejected"
	if accept {
		status = "confirmed"
	}

	en.store.SetRelation(strings.TrimSpace(child), strings.TrimSpace(parent), status)
}

// Propose generates parent relations for scope tags that have none yet (heuristic; the user confirms
// each yes/no). Returns the freshly-proposed relations.
func (en *Engine) Propose() []Relation {
	existing := map[string]bool{}
	for _, r := range en.store.Relations() {
		existing[r.Child] = true
	}

	tagset := map[string]bool{}
	root := ""
	hasBackend, hasFrontend := false, false

	for _, d := range en.store.Active() {
		for _, s := range d.Scope {
			t := strings.TrimSpace(s)
			tagset[t] = true

			switch {
			case strings.HasPrefix(t, "repo:"):
				root = t
			case strings.HasPrefix(t, "service:"):
				hasBackend = true
			case strings.HasPrefix(t, "frontend:"):
				hasFrontend = true
			}
		}
	}

	var props []Relation

	propose := func(child, parent string) {
		if child == "" || parent == "" || child == parent || existing[child] {
			return
		}

		en.store.SetRelation(child, parent, "proposed")
		props = append(props, Relation{Child: child, Parent: parent, Status: "proposed"})
		existing[child] = true
	}

	for t := range tagset {
		switch {
		case strings.HasPrefix(t, "service:"):
			propose(t, "layer:backend")
		case strings.HasPrefix(t, "frontend:"):
			propose(t, "layer:frontend")
		}
	}

	if hasBackend {
		propose("layer:backend", root)
	}

	if hasFrontend {
		propose("layer:frontend", root)
	}

	return props
}

// expandAncestors adds every confirmed ancestor of each context tag into the set, so a query scoped
// to a child also matches decisions scoped to its parents (inheritance down the tree).
func (en *Engine) expandAncestors(cset map[string]bool) {
	pm := map[string]string{}

	for _, r := range en.store.Relations() {
		if r.Status == "confirmed" {
			pm[r.Child] = r.Parent
		}
	}

	if len(pm) == 0 {
		return
	}

	seed := make([]string, 0, len(cset))
	for t := range cset {
		seed = append(seed, t)
	}

	for _, t := range seed {
		cur := t
		for i := 0; i < 32; i++ {
			p, ok := pm[cur]
			if !ok || p == "" || cset[p] {
				break
			}

			cset[p] = true
			cur = p
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
