package main

import (
	"context"
	"testing"
)

// hybridEngine returns an engine with the lexical hybrid enabled at weight w.
func hybridEngine(t *testing.T, w string) *Engine {
	t.Helper()

	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.05")
	c.Set("rank.w_lexical", w)

	return NewEngine(newMemStore(), newFakeEmbedder(), c)
}

// TestLexTokensKeepIdentifiersWhole is the crux of the entity fix: `orders` must survive as ONE
// token. Splitting it into "cost"/"records" recreates exactly the topical blur that made the dense
// retriever unable to tell the cost subsystem from the metrics one.
func TestLexTokensKeepIdentifiersWhole(t *testing.T) {
	got := lexTokens("ClickHouse is the sole store for orders — no MySQL dual-write")

	has := func(tok string) bool {
		for _, g := range got {
			if g == tok {
				return true
			}
		}

		return false
	}

	if !has("orders") {
		t.Fatalf("identifier must stay whole, got %v", got)
	}

	if has("the") {
		t.Errorf("stopword leaked through: %v", got)
	}

	// Negation words must NOT be stopped — telling "no dual-write" from "dual-write" is half the point.
	if !has("no") && !has("not") {
		t.Errorf("negation token was stripped, which defeats the purpose: %v", got)
	}
}

// TestHybridSeparatesConfusableEntities reproduces the real failure: two TRUE decisions about
// different tables that a dense-only retriever surfaces interchangeably, because both are "source of
// truth for a storage table" and the embedding cannot see which identifier they name.
func TestHybridSeparatesConfusableEntities(t *testing.T) {
	en := hybridEngine(t, "1.0")
	ctx := context.Background()

	cost, err := en.Capture(ctx, Decision{
		What: "ClickHouse is the sole store for orders with no MySQL dual write",
		Why:  "breakdown snapshots trends and anomaly reads all resolve from one source",
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics, err := en.Capture(ctx, Decision{
		What: "MySQL order_events is the source of truth and ClickHouse tables are replicas",
		Why:  "writes always go to both stores so a single authoritative store avoids divergence",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pad the corpus so BM25's idf has a real document collection to work against.
	for _, w := range []string{
		"retry failed calls with exponential backoff and jitter",
		"wrap downstream dependencies in a circuit breaker",
		"drain connections on graceful shutdown before exit",
		"rate limit requests with a token bucket per tenant",
	} {
		if _, err := en.Capture(ctx, Decision{What: w}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ query, want, wantNot string }{
		{"what is the source of truth for orders", cost.ID, metrics.ID},
		{"what is the source of truth for order_events", metrics.ID, cost.ID},
	} {
		out, err := en.Recall(ctx, []string{tc.query})
		if err != nil {
			t.Fatal(err)
		}

		if len(out) == 0 {
			t.Fatalf("%q: expected a result", tc.query)
		}

		if out[0].Decision.ID != tc.want {
			t.Errorf("%q: the decision naming that exact table must rank first; got %q",
				tc.query, out[0].Decision.What)
		}
	}
}

// TestHybridDisabledByDefault: the hybrid must be opt-in, so enabling it is a deliberate, measurable
// change rather than a silent shift in everyone's ranking.
func TestHybridDisabledByDefault(t *testing.T) {
	cands := []scored{
		{d: Decision{ID: "a"}, sim: 0.50, lex: 0.0},
		{d: Decision{ID: "b"}, sim: 0.40, lex: 99.0},
	}

	before := []float64{cands[0].sim, cands[1].sim}

	hybridRelevance(cands, NewConfig()) // no rank.w_lexical set

	if cands[0].sim != before[0] || cands[1].sim != before[1] {
		t.Fatalf("hybrid must be a no-op when disabled; got %v", []float64{cands[0].sim, cands[1].sim})
	}
}

// TestHybridPreservesRelevanceScale: fusion reorders, but the scores stay on the cosine scale, so the
// precision floor and the fitted noise model keep measuring the same quantity. Introducing a second
// relevance scale that only the ranker understands would silently break both.
func TestHybridPreservesRelevanceScale(t *testing.T) {
	// NOTE on list length: RRF's k=60 is a smoothing constant calibrated for long TREC-style result
	// lists, and on a 2–3 document list it dominates the rank differences almost entirely. A document
	// that wins the dense list by one rank and loses the lexical list by one rank is then EXACTLY
	// tied — which is correct RRF behaviour (the evidence really is balanced), but it means small
	// synthetic lists cannot demonstrate promotion. In production the candidate list is the whole
	// surfaceable corpus (~1.6k), where ranks span a wide range and the fusion discriminates properly.
	// The entity-conflation shape: "a" is the top DENSE hit but has no lexical support at all (it never
	// mentions the identifier the query names), while "b" is close behind on dense and dominant
	// lexically. Fusion should surface "b".
	cands := []scored{
		{d: Decision{ID: "a"}, sim: 0.90, lex: 0},  // dense 1st, lexical last
		{d: Decision{ID: "b"}, sim: 0.70, lex: 50}, // dense 2nd, lexical 1st
		{d: Decision{ID: "c"}, sim: 0.60, lex: 30},
		{d: Decision{ID: "d"}, sim: 0.50, lex: 20},
		{d: Decision{ID: "e"}, sim: 0.40, lex: 10},
	}

	c := NewConfig()
	c.Set("rank.w_lexical", "1.0")

	hybridRelevance(cands, c)

	got := map[float64]bool{}
	for _, s := range cands {
		got[s.sim] = true
	}

	for _, want := range []float64{0.90, 0.70, 0.60, 0.50, 0.40} {
		if !got[want] {
			t.Fatalf("fused scores must be a permutation of the original cosines; %v missing from %v",
				want, got)
		}
	}

	// "b" loses the dense list by 2 ranks but wins the lexical list outright, so it should be promoted.
	if cands[1].sim <= cands[0].sim {
		t.Errorf("decisive lexical evidence should promote b over a; got a=%.2f b=%.2f",
			cands[0].sim, cands[1].sim)
	}
}

// TestBM25IgnoresRetiredDecisions: the index must honour the same visibility rules as recall, or a
// superseded decision could be resurrected through the lexical channel.
func TestBM25IgnoresRetiredDecisions(t *testing.T) {
	ix := newBM25Index()
	ix.build([]Decision{
		{ID: "live", What: "clickhouse is the sole store for orders"},
		{ID: "retired", What: "clickhouse is the sole store for orders", SupersededBy: "live"},
		{ID: "quarantined", What: "clickhouse is the sole store for orders", Quarantined: true},
	})

	scores := ix.score("orders", NewConfig())
	if len(scores) != 1 {
		t.Fatalf("only the live decision may be indexed, got %d: %v", len(scores), scores)
	}

	if _, ok := scores["live"]; !ok {
		t.Errorf("the live decision should score, got %v", scores)
	}
}
