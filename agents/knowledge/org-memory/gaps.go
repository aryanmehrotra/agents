package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Gap detection — turning abstentions into a work list.
//
// WHY THIS IS THE HIGHEST-VALUE THING LEFT. Three of this project's flagship "retrieval defects"
// turned out, on investigation, not to be retrieval defects at all. The corpus holds eleven
// fail-closed decisions and not one mentions Redis; it holds zero decisions advising against retry.
// A cross-encoder was built, measured, and could not fix them, because **re-ranking reorders and
// cannot invent**. The memory was not failing to find the answer. It did not have the answer.
//
// That failure mode is invisible by construction. An abstention is a CORRECT output in this system —
// the honesty thesis depends on it — so "returned nothing" looks identical whether the corpus has a
// gap or the question was genuinely off-topic. Nothing distinguishes them, and nothing accumulates,
// so the same missing decision can be silently not-found a hundred times and leave no trace.
//
// The IR/search-analytics literature is blunt about this: a zero-result query is not a failure, it is
// a direct statement of what is absent, and repeated ones are demand evidence for content that does
// not exist yet. The standard practice is to log them, aggregate by frequency, threshold, and work
// the queue (Tunkelang, "Making Sense of Null and Low Results"; the same discipline underlies
// search-log-driven content strategy generally). The measured industry baseline — ~23% of queries
// return nothing while only ~8% of teams examine those logs — is precisely the asymmetry this file
// removes.
//
// WHAT MAKES A GAP DIFFERENT FROM NOISE. Not every empty result is a missing decision. "What is the
// capital of France" should return nothing forever. Two signals separate them, and both are already
// measured:
//
//   - NEARNESS — how close the best candidate came to the floor. A query that missed by 0.03 was
//     nearly answerable; one that missed by 0.15 is off-topic. `top_candidate_similarity` exists
//     precisely because the old code masked this to 0.0 and made the distinction unmeasurable.
//   - REPETITION — asked once is curiosity, asked repeatedly is a hole in the corpus. Frequency is
//     what turns an anecdote into evidence, and it is the signal a single request cannot carry.

// GapEntry is one repeatedly-unanswerable question.
type GapEntry struct {
	Query     string    `json:"query"`
	Count     int       `json:"count"`
	BestSim   float64   `json:"best_similarity"` // closest any decision came, across sightings
	Floor     float64   `json:"floor"`           // the bar it failed to clear
	ShortBy   float64   `json:"short_by"`        // floor − best: how near a miss it was
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Confirmed means a human answered the near-miss question with "yes, we have a rule for this" —
	// promoting it from a statistical near-miss to a stated missing decision.
	Confirmed bool `json:"confirmed"`
}

// gapLog accumulates abstentions, keyed by a normalised query so re-phrasings of the same question
// aggregate instead of each looking like a one-off.
//
// Bounded: a memory whose gap log grows without limit has traded one leak for another. When full it
// evicts the least useful entry — fewest sightings, then oldest — because the whole point is to
// surface the questions that keep coming back.
type gapLog struct {
	mu      sync.Mutex
	entries map[string]*GapEntry
	silent  map[string]bool // questions a human declared "correctly silent" — never ask again
	max     int
}

func newGapLog(max int) *gapLog {
	if max < 1 {
		max = 1
	}

	return &gapLog{entries: make(map[string]*GapEntry), silent: make(map[string]bool), max: max}
}

// record notes an abstention. `near` is the caller's judgement that this was close enough to be worth
// tracking at all — far-off-topic questions are correct silences and must not pollute the work list.
//
// `minTokens` is a substance bar, and it exists because nearness alone is not enough. Observed live:
// the four-word prompt "test it and fic it" — an instruction to an assistant, not a question about
// any org decision — landed on the capture work list at short_by 0.012, nearer the floor than a
// genuine gap. A work list that fills with fragments is a work list nobody reads.
//
// The obvious discriminator was tried first and REJECTED on measurement: `noise_p` is worse than
// useless here. That junk query scored noise_p = 2.2e-05 while the genuine gap
// ("kubernetes ingress nginx annotations") scored 7.1e-04 — the junk looked an order of magnitude
// MORE significant. That is the known relative-measure limitation of the fitted noise model
// (RESEARCH.md §11a): it compares a score to the query's OWN distribution, so a query whose
// distribution is tight reads as extreme regardless of whether it means anything.
//
// So the bar is content substance: distinct non-stopword tokens, counted with the same tokenizer the
// lexical index uses. This is an explicit HEURISTIC, not a fitted quantity — it is a knob, it is
// labelled as chosen rather than measured, and it is the crude proxy available for "is this actually
// an information need". Better would be intent classification, which Phase 0 does not have.
func (g *gapLog) record(query string, best, floor float64, near bool, minTokens int, now time.Time) bool {
	q := normalizeGapQuery(query)
	if q == "" || !near {
		return false
	}

	if substanceTokens(q) < minTokens {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.silent[q] {
		return false // a human already said this silence is correct; asking again wastes their patience
	}

	if e, ok := g.entries[q]; ok {
		e.Count++
		e.LastSeen = now

		if best > e.BestSim { // keep the closest sighting — the phrasing that nearly worked
			e.BestSim, e.Floor, e.ShortBy = best, floor, round(floor-best)
		}

		return true
	}

	if len(g.entries) >= g.max {
		g.evictLocked()
	}

	g.entries[q] = &GapEntry{
		Query: query, Count: 1,
		BestSim: best, Floor: floor, ShortBy: round(floor - best),
		FirstSeen: now, LastSeen: now,
	}

	return true
}

// evictLocked drops the least-evidenced entry: fewest sightings first, oldest as the tie-break.
func (g *gapLog) evictLocked() {
	var (
		worstKey string
		worst    *GapEntry
	)

	for k, e := range g.entries {
		// Never evict a human-confirmed gap. Report() promises a confirmed entry "outranks any amount
		// of statistical evidence", but eviction ranked purely on Count — so a resurrected
		// yes-after-no entry (Count 1) was silently dropped by a handful of anonymous one-off
		// near-misses, which any unauthenticated GET /recall can manufacture. The one entry a person
		// explicitly promoted is the one the bound must protect.
		if e.Confirmed {
			continue
		}

		if worst == nil || e.Count < worst.Count || (e.Count == worst.Count && e.LastSeen.Before(worst.LastSeen)) {
			worstKey, worst = k, e
		}
	}

	if worstKey != "" {
		delete(g.entries, worstKey)
	}
}

// Report returns the gaps worth acting on, most compelling first.
//
// Ranked by sightings, then by how near the miss was. Frequency leads because it is the signal that
// separates a hole in the corpus from someone's one-off curiosity, and a near miss with one sighting
// is not yet evidence of anything.
func (g *gapLog) Report(minCount, limit int) []GapEntry {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]GapEntry, 0, len(g.entries))

	for _, e := range g.entries {
		if e.Count >= minCount {
			out = append(out, *e)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		// A human saying "yes, we have a rule for this" outranks any amount of statistical evidence:
		// it is a stated fact about the corpus, not an inference from similarity scores.
		if out[i].Confirmed != out[j].Confirmed {
			return out[i].Confirmed
		}

		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}

		return out[i].ShortBy < out[j].ShortBy
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	return out
}

// substanceTokens counts how many words in a query carry real content.
//
// It deliberately does NOT reuse lexTokens. The first attempt at this bar raised lexTokens' minimum
// token length from 2 to 3 so that "it"/"is" would stop counting as substance — which silently
// dropped "no" from the LEXICAL INDEX, defeating the exact property that index exists for (telling
// "no dual-write" from "dual-write"), broke its own committed test, and also erased two-character
// identifiers like `s3`, `db` and `ci` from BM25. A gap-list heuristic has no business changing how
// retrieval tokenizes; the concerns are separate and now the code says so.
func substanceTokens(q string) int {
	n := 0

	for _, t := range lexTokens(q) {
		if len(t) > 2 { // "it", "we", "do" are filler HERE — but must stay in the retrieval index
			n++
		}
	}

	return n
}

// normalizeGapQuery collapses trivial phrasing differences so "should we fail closed?" and "Should we
// fail closed" aggregate. Deliberately shallow — lowercase, strip punctuation, collapse whitespace.
// Anything cleverer (stemming, embedding-clustering) would merge questions that are genuinely
// different, and a gap list that lies about its counts is worse than one that under-merges.
func normalizeGapQuery(q string) string {
	var b strings.Builder

	b.Grow(len(q))

	space := false

	for _, r := range strings.ToLower(strings.TrimSpace(q)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)

			space = false
		default:
			if !space && b.Len() > 0 {
				b.WriteByte(' ')

				space = true
			}
		}
	}

	return strings.TrimSpace(b.String())
}

// recentQueries remembers which question last surfaced each decision, so a later "wrong" verdict can
// be attributed back to the question that provoked it.
//
// THIS IS THE OTHER HALF OF GAP DETECTION, and the harder half. An abstention announces itself — the
// result set is empty and `top_candidate_similarity` says how near it came. A CONFIDENT WRONG ANSWER
// announces nothing. Measured on this corpus, "should we fail closed when redis is down" returns the
// "fail open" rule with weak=false and noise_p = 4e-13: the system is as certain as it is capable of
// being, while giving precisely the opposite advice, because the corpus contains no fail-closed rule
// for Redis and the nearest thing is its exact inverse.
//
// No confidence signal can catch that. The score distribution is not confused; the retrieval is not
// ambiguous; there is simply nothing correct to find and something incorrect that fits beautifully.
// The ONLY channel that reveals it is a human saying "wrong" — which is exactly why the ask loop
// samples confident recalls at all (uncertainty sampling is structurally blind here, ask.go), and why
// that label has to flow back into the gap list rather than only nudging a ranking weight.
type recentQueries struct {
	mu  sync.Mutex
	by  map[string]string // decision id → the query that surfaced it
	ord []string          // insertion order, for bounded eviction
	max int
}

func newRecentQueries(max int) *recentQueries {
	if max < 1 {
		max = 1
	}

	return &recentQueries{by: make(map[string]string), max: max}
}

func (r *recentQueries) note(decisionID, query string) {
	if decisionID == "" || strings.TrimSpace(query) == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, seen := r.by[decisionID]; !seen {
		if len(r.ord) >= r.max {
			delete(r.by, r.ord[0])
			r.ord = r.ord[1:]
		}

		r.ord = append(r.ord, decisionID)
	}

	r.by[decisionID] = query
}

func (r *recentQueries) get(decisionID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	q, ok := r.by[decisionID]

	return q, ok
}

// resolve answers a near-miss question. `haveRule` true means the org DOES have this decision and it
// simply was not found — the gap is real and worth capturing. False means the silence was correct,
// and the entry is dropped so the same non-question stops being asked about.
//
// This exists because the near-miss prompt was originally unanswerable: it rendered two options
// ("yes — we have a rule for this" / "no — correctly silent") while the only feedback endpoint
// required a decision_id, and an abstention has no decision. Asking a question with no channel to
// receive the answer is worse than not asking — it trains the reader to ignore the prompt.
func (g *gapLog) resolve(query string, haveRule bool, now time.Time) bool {
	q := normalizeGapQuery(query)
	if q == "" {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.entries[q]

	if !ok {
		// A previously-silenced question has no entry, because silencing suppresses recording. A later
		// "yes, we DO have this rule" must still override that verdict, or the first "no" would be
		// irreversible and an operator could never correct themselves.
		if haveRule && g.silent[q] {
			delete(g.silent, q)

			g.entries[q] = &GapEntry{
				Query: query, Count: 1, Confirmed: true,
				FirstSeen: now, LastSeen: now,
			}

			return true
		}

		return false
	}

	if !haveRule {
		// Remember the verdict, not just the deletion. Deleting alone was not enough: the near-miss
		// prompt fires on any near-miss abstention, so the very next identical query re-recorded the
		// entry and re-asked the question. A "correctly silent" answer that does not stop the asking
		// is not an answer, it is a loop.
		delete(g.entries, q)
		g.silent[q] = true

		return true
	}

	delete(g.silent, q) // a later "yes" overrides an earlier "no"

	e.Confirmed = true
	e.LastSeen = now

	return true
}

// silenced reports whether a human already declared this question a correct silence.
func (g *gapLog) silenced(query string) bool {
	q := normalizeGapQuery(query)
	if q == "" {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	return g.silent[q]
}
