package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	errEmptyWhat        = errors.New("decision `what` is required")
	errNoDecision       = errors.New("feedback requires `decision_id`")
	errBadSignal        = errors.New("unknown feedback signal")
	errSupersedeIDs     = errors.New("supersede requires both `old` and `new` decision ids")
	errSupersedeSelf    = errors.New("a decision cannot supersede itself")
	errSupersedeRetired = errors.New("cannot supersede into a retired decision")
	errUnknownDecision  = errors.New("unknown decision")
)

// Engine is the org-memory core: capture → store → recall → advise, plus the feedback loop. Every
// dependency is an interface (Store, Embedder, Config), so the engine is fully unit-testable with no
// infra and every backing is swappable. Phase 0 is advisory-only — it surfaces prior decisions as
// context and learns *relevance* from feedback; it never asserts correctness.
type Engine struct {
	store Store
	embed Embedder
	cfg   *Config

	lex      *bm25Index // lexical half of hybrid retrieval; nil-safe, rebuilt on corpus change
	lexDirty atomic.Bool

	rr      Reranker       // optional cross-encoder stage (nil = dense-only); see rerank.go
	gaps    *gapLog        // repeatedly-unanswerable questions — the corpus's own work list
	recent  *recentQueries // decision → the query that surfaced it, so a later "wrong" attributes back
	health  *recallHealth  // rolling confident-recall ratio — the signal the ask-rate loop controls on
	sampler *askSampler    // spot-check cadence for CONFIDENT recalls (accumulator, not a modulus)

	// live counters (since start) for the value metrics
	recalls      int64
	recallsEmpty int64
	surfaced     int64
}

func NewEngine(s Store, e Embedder, cfg *Config) *Engine {
	en := &Engine{
		store: s, embed: e, cfg: cfg, lex: newBM25Index(),
		health: newRecallHealth(cfg.I("feedback.health_window", 200)), sampler: &askSampler{},
		gaps:   newGapLog(cfg.I("gaps.max_tracked", 500)),
		recent: newRecentQueries(cfg.I("gaps.recent_window", 2000)),
	}
	en.lexDirty.Store(true)
	en.loadGaps()

	return en
}

// loadGaps restores the work list from durable storage. Silent on absence or corruption: a memory
// that refuses to boot because a secondary index is unreadable is worse than one that starts empty.
func (en *Engine) loadGaps() {
	raw := en.store.GetMeta(gapMetaKey)
	if raw == "" {
		return
	}

	var snap gapSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return
	}

	en.gaps.restore(snap)
}

// saveGaps persists the work list. Called after every change rather than on a timer, because the
// thing being protected is exactly the state lost to an unplanned restart — and the payload is a few
// KB, bounded by gaps.max_tracked.
func (en *Engine) saveGaps() {
	b, err := json.Marshal(en.gaps.snapshot())
	if err != nil {
		return
	}

	en.store.SetMeta(gapMetaKey, string(b))
}

// RecallHealth reports the rolling fraction of recalls that came back confident, and the sample
// behind it. This is the loop variable for the feedback ask-rate: when it falls below target the
// system asks more questions, and stops again on its own once it recovers.
func (en *Engine) RecallHealth() (ratio float64, n int) { return en.health.ratio() }

// Recalls returns the since-start recall count — the deterministic counter used to sample confident
// recalls for spot checks, instead of an RNG that no test could reproduce.
func (en *Engine) Recalls() int64 { return atomic.LoadInt64(&en.recalls) }

// Labels counts the feedback signals recorded so far, across every signal type.
func (en *Engine) Labels() int {
	v := en.ValueStats()
	return v.Helpful + v.Used + v.NotRelevant + v.Wrong
}

// relevanceScopeKey is where a query-scoped relevance judgement is counted.
//
// `not_relevant` is a claim about a query↔decision PAIR, not about the decision (Saracevic, JASIST
// 2007 — relevance is a relation, never a property), so it has to be stored against the pair. The
// stats table is keyed by a bare string with no foreign key to decisions, so a composite key scopes
// the counter with no change to the Store interface and no migration.
func relevanceScopeKey(decisionID, scope string) string {
	return decisionID + "\x00nr\x00" + scope
}

// feedbackScope decides WHICH information need a relevance judgement belongs to.
//
// Preference order: the caller's explicit `context` — the field that has existed on Feedback since
// Phase 0 and was never read — then the query that actually surfaced this decision, which the engine
// already remembers for gap attribution. Empty means the judgement cannot be attributed at all.
//
// Matching is on NORMALISED query text, so the same question asked the same way transfers and a
// different phrasing does not. Deliberately conservative: over-applying a demotion is the bug being
// fixed (one bad match suppressed a decision everywhere), while under-applying merely costs a little
// signal — so when in doubt, the demotion does not travel.
func (en *Engine) feedbackScope(f Feedback) string {
	if len(f.Context) > 0 {
		// Strip scope tags. Ranking looks this up with the EMBEDDED text, which never contains tags
		// (they are matched, not embedded), so keying on the raw context array meant a caller who
		// echoed back its recall context — the natural, documented usage — wrote the counter under a
		// key nothing ever reads. A silent no-op is the worst outcome for a feedback signal: the
		// caller is told it succeeded and nothing changes.
		var text []string

		for _, c := range f.Context {
			if !isScopeTag(c) && strings.TrimSpace(c) != "" {
				text = append(text, c)
			}
		}

		if scope := normalizeGapQuery(strings.Join(text, " ")); scope != "" {
			return scope
		}
	}

	if q, ok := en.recent.get(f.DecisionID); ok {
		return normalizeGapQuery(q)
	}

	return ""
}

// scopedNotRelevant is how many times THIS decision was marked not-relevant for THIS information
// need. Ranking demotes on this instead of the lifetime total, so a judgement about one question can
// no longer bury the decision for every other question.
func (en *Engine) scopedNotRelevant(decisionID, query string) int {
	scope := normalizeGapQuery(query)
	if scope == "" {
		return 0
	}

	return en.store.Stats(relevanceScopeKey(decisionID, scope)).NotRelevant
}

// lexicalScores returns BM25 scores for the query, rebuilding the index first if the corpus changed.
// Rebuild is lazy and happens on the read path so a burst of captures costs one rebuild, not one per
// write — and so a deployment that never enables the hybrid never pays for the index at all.
func (en *Engine) lexicalScores(query string, chain ...string) map[string]float64 {
	if en.lex == nil {
		return nil
	}

	if en.lexDirty.CompareAndSwap(true, false) {
		en.lex.build(en.store.Active())
	}

	return en.lex.score(query, en.cfg, chain...)
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

		// Honour `supersedes` on the REINFORCE path too. This branch used to return here, before the
		// supersede branch further down, so re-capturing an already-known decision with `supersedes`
		// set was silently a no-op: no error, no supersede, caller told it succeeded. That is the
		// worst failure shape for an override — the operator believes the stale decision is retired
		// and it keeps being surfaced. Content-addressed ids make this the COMMON case, not an edge
		// case: restating a known rule to retire an old one hashes to the existing id and lands here.
		en.applySupersede(&existing, in.Supersedes)

		en.store.Put(existing)
		en.lexDirty.Store(true)

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

	en.applySupersede(&d, in.Supersedes)

	en.store.Put(d)
	en.linkScopeEdges(d)
	en.lexDirty.Store(true)

	// Close any gap this decision now answers. The work list exists to be emptied, and a question that
	// has just been written down should stop being asked about without the author having to remember
	// to say so. See gaps.closeAnswered for why neither existing resolve path could express this.
	if closed := en.gaps.closeAnswered(emb); len(closed) > 0 {
		en.saveGaps()
	}

	return d, nil
}

// applySupersede retires `oldID` in favour of d, on both the new-decision and the reinforce path.
// Shared so the two paths cannot drift apart again — the drift was the defect. Self-supersede is
// refused: it would set SupersededBy on the decision itself and erase it from recall entirely.
func (en *Engine) applySupersede(d *Decision, oldID string) {
	s := strings.TrimSpace(oldID)
	if s == "" || s == d.ID {
		return
	}

	if en.store.Supersede(s, d.ID) {
		d.Supersedes = firstNonEmpty(d.Supersedes, s)
	}
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

	// Cap the calibrated floor. Without a ceiling on the ceiling, a corpus whose noise probes score
	// very high would calibrate a floor that nothing can clear — the system would abstain on
	// everything, which reads as "working correctly" (abstention IS a valid output) while actually
	// being dead. The cap is policy: how much silence an org will tolerate before it wants to be told
	// its corpus is unusable, so it is a knob, not a literal.
	floor = ceiling + margin
	if maxFloor := en.cfg.F("retrieve.max_precision_floor", 0.99); floor > maxFloor {
		floor = maxFloor
	}

	en.cfg.Set("retrieve.precision_floor", strconv.FormatFloat(floor, 'f', 4, 64))

	return floor, ceiling, nil
}

// Recall returns the few prior decisions worth surfacing for a context — or nothing. Dense cosine
// over scope-matched, non-superseded candidates, then rankAndFilter (precision floor + ordering +
// feedback nudge). The chain (person/team scopes) lets config knobs resolve most-specific-first.
// Recall returns only the surfaced items — the common path, for callers that don't need to reason
// about WHY a recall came back empty. See RecallWithDiag for the measured abstention.
func (en *Engine) Recall(ctx context.Context, queryContext []string, chain ...string) ([]RecalledItem, error) {
	items, _, err := en.RecallWithDiag(ctx, queryContext, chain...)
	return items, err
}

// RecallWithDiag is Recall plus the measurements that make an EMPTY result legible: the best
// similarity any surfaceable decision achieved before the gate, and the floor it failed to clear.
func (en *Engine) RecallWithDiag(ctx context.Context, queryContext []string, chain ...string) ([]RecalledItem, RecallDiag, error) {
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

	// No real query content → nothing to match on semantically. Embedding an empty or
	// punctuation-only string yields a near-constant vector that spuriously "matches" a fixed few
	// decisions, so a blank / whitespace / symbols-only / scope-only query must return NOTHING. We
	// require at least one alphanumeric character before attempting semantic recall.
	queryText := strings.TrimSpace(strings.Join(textParts, " "))
	if !hasAlnum(queryText) {
		atomic.AddInt64(&en.recalls, 1)
		atomic.AddInt64(&en.recallsEmpty, 1)

		return nil, RecallDiag{}, nil
	}

	qvec, err := en.embed.Embed(ctx, queryText, RoleQuery)
	if err != nil {
		return nil, RecallDiag{}, err
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
	hardFacets := hardFilterTags(tagParts, en.cfg, chain...)

	guardPolarity := en.cfg.F("rank.polarity_guard", 1, chain...) > 0

	// ENTITY GUARD. Only meaningful when the query actually names a subject the corpus facets on —
	// otherwise every scoped decision would be dropped for every unscoped question.
	entityPrefixes := splitCSV(en.cfg.Str("rank.entity_facets", "table:,service:,entity:", chain...))
	qTokens := set(lexTokens(strings.ToLower(queryText)))

	corpusEntities := map[string]bool{}

	for _, d := range en.store.Active() {
		for _, sc := range d.Scope {
			if p, v, ok := strings.Cut(strings.ToLower(sc), ":"); ok && containsStr(entityPrefixes, p+":") {
				corpusEntities[v] = true
			}
		}
	}

	guardEntity := en.cfg.F("rank.entity_guard", 1, chain...) > 0 && queryNamesEntity(qTokens, corpusEntities)

	// Isolation is checked from the DATA side and is not configurable — see isolationOK.
	queryTags := lowerTagSet(tagParts)

	// Lexical half of hybrid retrieval — computed on the same query TEXT the dense side embeds, so
	// the two signals answer the same question by different means. Empty unless rank.w_lexical > 0.
	lex := map[string]float64{}
	if en.cfg.F("rank.w_lexical", 0, chain...) > 0 {
		lex = en.lexicalScores(queryText, chain...)
	}

	// SCAN BUDGET. The exact scan is O(N·dim) and measured: ~1.1 ms at 1k decisions, 12 ms at 10k,
	// 87 ms at 25k (superlinear past ~10⁴ as the vectors leave cache). §1.7 requires "recall p95 <
	// budget · budget-blown → falls back, NEVER blocks", and the failure mode it guards against is the
	// one that matters for an advisory system: org-memory sits in an agent's prompt path, so a slow
	// recall does not degrade an answer, it stalls the agent. Partial context on time beats complete
	// context late — the standard tail-latency posture for a request that is one component of a larger
	// response (Dean & Barroso, "The Tail at Scale", CACM 2013: return what you have when the deadline
	// arrives; a synchronous fan-in is only as fast as its slowest part).
	//
	// Degradation is by TRUNCATION of the scan, and it is honest about itself: the caller is told the
	// budget was hit and how much of the corpus was searched, because a silently partial recall would
	// let an abstention ("nothing relevant") stand in for a timeout ("we did not look"). Those two
	// must never be confused in a system whose product is knowing when it does not know.
	budget := time.Duration(en.cfg.F("retrieve.budget_ms", 0, chain...) * float64(time.Millisecond))
	deadline := time.Now().Add(budget)

	var (
		cands    []scored
		scanned  int
		exceeded bool
	)

	active := en.store.Active()

	for i, d := range active {
		// Check the clock every 256 decisions: often enough to bound the overrun to well under a
		// millisecond at measured scan rates, rare enough that the check itself is not the cost.
		if budget > 0 && i%256 == 0 && i > 0 && time.Now().After(deadline) {
			exceeded = true
			break
		}

		scanned++

		if !isolationOK(d.Scope, queryTags) {
			continue // tenant/org boundary — checked first, and never bypassable by config
		}

		if !hasAllTags(d.Scope, hardFacets) {
			continue // failed a hard facet predicate — cannot be correct for this request
		}

		ok, spec := scopeMatch(d.Scope, cset)
		if hardScope && !ok {
			continue
		}

		// POLARITY GUARD. Drop a decision that asserts the OPPOSITE of what was asked. Returning the
		// exact reverse of the question is the worst output this system can produce, and it arrives
		// with maximum confidence — no similarity or confidence signal can see it, because the two
		// texts are topically identical and differ by one token. Silence is a correct answer here.
		if guardPolarity && polarityConflict(queryText, d.What+" "+d.Why, en.cfg, chain...) {
			continue
		}

		// A decision faceted on a different subject than the one asked about is not an answer to
		// this question, however similar it reads.
		if guardEntity && entityConflict(qTokens, d.Scope, entityPrefixes) {
			continue
		}

		cands = append(cands, scored{
			d:    d,
			sim:  cosine(qvec, d.Embedding),
			lex:  lex[d.ID],
			spec: spec, // 0 when out of context → no context boost, but still eligible on relevance
			st:   en.scopedStats(d.ID, queryText),
		})
	}

	hybridRelevance(cands, en.cfg, chain...)

	// CASCADE. With a re-ranker attached, the cosine stage becomes pure RECALL — wide and ungated —
	// and the cross-encoder decides both order and relevance. Gating on cosine FIRST cannot work, and
	// the measurement is unambiguous: for "should we fail closed when redis is down" only 3 decisions
	// cleared the 0.653 floor, and not one of the corpus's 11 fail-closed rules was among them. The
	// bi-encoder discards polarity mismatches before the stage that can see polarity ever runs. A
	// cascade cannot rescue what its first stage threw away (Wang/Lin/Metzler, SIGIR 2011: the cheap
	// stage is tuned for recall, the expensive one supplies precision).
	items, reranked := rerankCandidates(ctx, en.rr, queryText, cands, en.cfg, chain...)

	floor, noise := gateFloorWithNoise(cands, en.cfg, chain...)

	if !reranked {
		items, floor, noise = rankAndFilter(cands, en.cfg, chain...)
	}

	atomic.AddInt64(&en.recalls, 1)
	atomic.AddInt64(&en.surfaced, int64(len(items)))

	if len(items) == 0 {
		atomic.AddInt64(&en.recallsEmpty, 1)
	}

	// Measure the abstention instead of masking it. The best PRE-gate cosine is what makes an empty
	// result interpretable — "the closest thing in memory scored 0.58 against a 0.65 bar" is a
	// calibration datum; reporting 0.0 (the old behaviour, an artifact of taking the max over an
	// already-empty list) is not, and it silently invites false confidence in the abstention margin.
	// Only surfaceable decisions count as background, matching gateFloor's own definition.
	diag := RecallDiag{
		Floor: floor, Candidates: len(cands), NoiseP: 1, NoiseFitted: noise.ok,
		Scanned: scanned, CorpusSize: len(active), BudgetExceeded: exceeded,
	}

	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue
		}

		if c.sim > diag.TopSimilarity {
			diag.TopSimilarity = c.sim
		}
	}

	// Confidence as a probability, from the SAME model that made the admit/reject call. Deriving the
	// gate and the confidence signal from two unrelated heuristics is how the old `weak` flag ended up
	// contradicting the gate — firing on 16/32 answers the gate had just accepted.
	diag.NoiseP = noise.tailProb(diag.TopSimilarity)

	// An abstention that nearly landed is the corpus telling us what it is missing. Recorded here
	// rather than in the handler, so every caller — HTTP, the MCP tool, the recall hook — feeds the
	// same work list. See gaps.go for why "nearly" is the load-bearing word.
	if len(items) == 0 {
		diag.GapRecorded = en.recordGap(queryText, qvec, diag, chain...)
	}

	// Remember what question surfaced each decision. If a human later marks one WRONG, that is the
	// only evidence available that the corpus had no right answer — see recentQueries.
	for _, it := range items {
		en.recent.note(it.Decision.ID, queryText)
	}

	return items, diag, nil
}

// Supersede retires an EXISTING decision in favour of an existing replacement — the retroactive half
// of the supersede primitive.
//
// The store has always been able to do this (Store.Supersede) and recall has always honoured it, but
// until now the only way to reach it was to CAPTURE a new decision carrying `supersedes`. That covers
// "I am writing down the new rule", and nothing else. It does not cover the case that actually
// accumulates in a real corpus: two decisions already recorded, independently, from different sources,
// which turn out to conflict — the exact shape of the cost-storage cluster that surfaced contradictory
// advice under a 9.4% contradiction rate. Those decisions could not be reconciled at all, because the
// primitive that fixes them was unreachable. The root cause was never a missing schema field; it was
// a missing verb.
//
// Refuses to link into an already-retired decision: superseding A into B when B is itself superseded
// would hide A behind a head that recall never surfaces, silently deleting advice in a system whose
// central promise is that nothing is ever deleted.
func (en *Engine) Supersede(oldID, newID string) error {
	oldID, newID = strings.TrimSpace(oldID), strings.TrimSpace(newID)

	if oldID == "" || newID == "" {
		return errSupersedeIDs
	}

	if oldID == newID {
		return errSupersedeSelf
	}

	old, ok := en.store.Get(oldID)
	if !ok {
		return fmt.Errorf("%w: %q", errUnknownDecision, oldID)
	}

	replacement, ok := en.store.Get(newID)
	if !ok {
		return fmt.Errorf("%w: %q", errUnknownDecision, newID)
	}

	if old.SupersededBy != "" {
		return fmt.Errorf("%q is already superseded by %q", oldID, old.SupersededBy)
	}

	if replacement.SupersededBy != "" {
		return fmt.Errorf("%w: %q is itself superseded by %q", errSupersedeRetired, newID, replacement.SupersededBy)
	}

	en.lexDirty.Store(true)

	if !en.store.Supersede(oldID, newID) {
		return fmt.Errorf("%w: %q", errUnknownDecision, oldID)
	}

	// Record the backward link on the replacement too, so the pair is navigable from either end.
	// `supersedes` holds a single id, so when one decision retires several predecessors only the first
	// is named here; SupersededBy on each retired row is the authoritative edge and the one recall
	// filters on, so no history is lost either way.
	if replacement.Supersedes == "" {
		replacement.Supersedes = oldID
		replacement.Updated = time.Now()
		en.store.Put(replacement)
	}

	return nil
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
		en.store.Bump(f.DecisionID, f.Signal, 1) // lifetime total — reporting only

		// A relevance judgement also belongs to the information need it was made about. Ranking reads
		// THIS counter, not the lifetime one, so "bad answer to question A" can no longer suppress the
		// decision for question B. An unattributable judgement is deliberately not applied to ranking
		// at all: a relation with no second term cannot be acted on.
		if f.Signal == "not_relevant" {
			if scope := en.feedbackScope(f); scope != "" {
				en.store.Bump(relevanceScopeKey(f.DecisionID, scope), f.Signal, 1)
			}
		}
	case "wrong", "outdated":
		en.store.RecordFeedback(f)
		en.store.Bump(f.DecisionID, f.Signal, 1)

		// A WRONG verdict on a confidently-surfaced decision is the only evidence that the corpus had
		// no right answer for that question — no confidence signal can detect it, because the system
		// is not confused, it is confidently serving the nearest wrong thing. Attribute it back to the
		// question and put it on the capture work list. See recentQueries in gaps.go.
		if q, ok := en.recent.get(f.DecisionID); ok {
			if en.gaps.record(q, nil, 0, 0, true, en.cfg.I("gaps.min_content_tokens", 3), time.Now().UTC()) {
				en.saveGaps()
			}
		}

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
// List returns the decisions visible to a caller asserting `tags`.
//
// Isolation applies HERE TOO. It did not, and that was the strongest counterexample to this system's
// "isolation is not bypassable" claim: /recall correctly refused a decision scoped `org:acme` to an
// unscoped caller, while /decisions — an unauthenticated GET, the dashboard's "whole brain" view —
// handed the same decision to anyone who asked. Enforcing a boundary on the query path and not on the
// bulk-read path enforces nothing; an attacker uses the endpoint that does not check.
func (en *Engine) List(tags ...string) []DecisionView {
	queryTags := lowerTagSet(tags)

	act := en.store.Active()
	out := make([]DecisionView, 0, len(act))

	for _, d := range act {
		if !isolationOK(d.Scope, queryTags) {
			continue
		}

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

	// Depth bound on the ancestor walk. `cset[p]` already breaks true cycles, so this is not the
	// cycle guard — it bounds how far a query inherits UP the scope tree, which is a modelling
	// choice about how deep an org's hierarchy is meaningful, not a safety constant.
	maxDepth := en.cfg.I("hierarchy.max_inherit_depth", 32)

	for _, t := range seed {
		cur := t
		for i := 0; i < maxDepth; i++ {
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

// WithReranker attaches an optional cross-encoder stage. Injected rather than constructed internally
// so ranking stays unit-testable without a 635 MB model, and so the model is a deployment choice
// rather than a compiled-in one.
func (en *Engine) WithReranker(rr Reranker) *Engine {
	en.rr = rr
	return en
}

// Gaps returns the questions this memory repeatedly could not answer — the capture work list.
func (en *Engine) Gaps(minCount, limit int) []GapEntry { return en.gaps.Report(minCount, limit) }

// recordGap notes an abstention when it was a NEAR miss. Far-off-topic silences are correct and must
// not pollute the list: the value of a gap report is entirely in its signal-to-noise.
// recordGap returns whether the question actually LANDED on the work list. Callers use the return
// value instead of re-deriving "is this worth asking about" — see RecallDiag.GapRecorded.
func (en *Engine) recordGap(query string, qvec []float32, diag RecallDiag, chain ...string) bool {
	near := diag.TopSimilarity > 0 &&
		diag.Floor-diag.TopSimilarity < en.cfg.F("gaps.near_miss", 0.08, chain...)

	recorded := en.gaps.record(query, qvec, round(diag.TopSimilarity), round(diag.Floor), near,
		en.cfg.I("gaps.min_content_tokens", 3, chain...), time.Now().UTC())
	if recorded {
		en.saveGaps()
	}

	return recorded
}

// scopedStats is a decision's feedback as RANKING should see it: positive signals and `wrong` are
// properties of the decision and stay lifetime totals, while `not_relevant` is narrowed to judgements
// made about this same information need. Mixing the two scales was the category error.
func (en *Engine) scopedStats(decisionID, query string) Stats {
	st := en.store.Stats(decisionID)
	st.NotRelevant = en.scopedNotRelevant(decisionID, query)

	return st
}

// ResolveGap records a human answer to a near-miss question. Returns false when the query is not on
// the list (already resolved, evicted, or never recorded).
func (en *Engine) ResolveGap(query string, haveRule bool) bool {
	ok := en.gaps.resolve(query, haveRule, time.Now().UTC())
	if ok {
		en.saveGaps()
	}

	return ok
}
