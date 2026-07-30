package main

import (
	"testing"
	"time"
)

// TestGapLogOnlyTracksNearMisses: the value of a gap report is entirely its signal-to-noise. "What is
// the capital of France" should return nothing forever and must never appear on a capture work list.
func TestGapLogOnlyTracksNearMisses(t *testing.T) {
	g := newGapLog(100)
	now := time.Now()

	g.record("should we fail closed when redis is down", 0.62, 0.65, true, 3, now)
	g.record("what is the capital of France", 0.20, 0.65, false, 3, now)

	got := g.Report(1, 0)
	if len(got) != 1 {
		t.Fatalf("only the near miss belongs on the list, got %d: %+v", len(got), got)
	}

	if got[0].ShortBy < 0.029 || got[0].ShortBy > 0.031 {
		t.Errorf("short_by should record how near the miss was (0.03), got %.4f", got[0].ShortBy)
	}
}

// TestGapLogRanksByRepetition: asked once is curiosity, asked repeatedly is a hole in the corpus.
// Frequency is what turns an anecdote into evidence, so it must lead the ranking.
func TestGapLogRanksByRepetition(t *testing.T) {
	g := newGapLog(100)
	now := time.Now()

	for i := 0; i < 5; i++ {
		g.record("do not retry failed requests", 0.60, 0.65, true, 3, now)
	}

	g.record("something asked once", 0.649, 0.65, true, 3, now) // a much nearer miss, but seen once

	got := g.Report(1, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 gaps, got %d", len(got))
	}

	if got[0].Count != 5 {
		t.Errorf("the repeatedly-asked question must rank first, got %q (count %d)", got[0].Query, got[0].Count)
	}

	// min_count is the threshold that keeps one-offs out of the work list.
	if only := g.Report(2, 0); len(only) != 1 {
		t.Errorf("min_count=2 should drop the single sighting, got %d", len(only))
	}
}

// TestGapLogAggregatesPhrasings: re-phrasings of the same missing decision must accumulate into one
// entry, or a real gap looks like several unrelated one-offs and never crosses the threshold.
func TestGapLogAggregatesPhrasings(t *testing.T) {
	g := newGapLog(100)
	now := time.Now()

	g.record("Should we fail closed for redis?", 0.60, 0.65, true, 3, now)
	g.record("should we fail closed for redis", 0.62, 0.65, true, 3, now)
	g.record("  SHOULD WE FAIL CLOSED FOR REDIS!!  ", 0.61, 0.65, true, 3, now)

	got := g.Report(1, 0)
	if len(got) != 1 {
		t.Fatalf("three phrasings of one question should aggregate, got %d: %+v", len(got), got)
	}

	if got[0].Count != 3 {
		t.Errorf("expected count 3, got %d", got[0].Count)
	}

	// The closest sighting is kept — that is the phrasing that nearly worked.
	if got[0].BestSim != 0.62 {
		t.Errorf("should keep the best similarity across sightings, got %.3f", got[0].BestSim)
	}
}

// TestGapLogIsBounded: a memory whose gap log grows without limit has traded one leak for another.
// Eviction must drop the LEAST-evidenced entry, never the repeatedly-asked one.
func TestGapLogIsBounded(t *testing.T) {
	g := newGapLog(3)
	now := time.Now()

	for i := 0; i < 4; i++ {
		g.record("important recurring question", 0.62, 0.65, true, 3, now.Add(time.Duration(i)*time.Second))
	}

	for _, q := range []string{"one off a", "one off b", "one off c", "one off d"} {
		g.record(q, 0.62, 0.65, true, 1, now)
	}

	got := g.Report(1, 0)
	if len(got) > 3 {
		t.Fatalf("log must stay bounded at 3, got %d", len(got))
	}

	for _, e := range got {
		if e.Count == 4 {
			return // the recurring one survived eviction
		}
	}

	t.Error("eviction dropped the most-evidenced gap — exactly the one worth keeping")
}

// TestGapLogRejectsInsubstantialQueries: nearness alone is not enough. Observed live, the four-word
// prompt "test it and fic it" — an instruction to an assistant, not a question about any org decision
// — landed on the capture work list at short_by 0.012, NEARER the floor than a genuine gap. A work
// list that fills with fragments is a work list nobody reads.
func TestGapLogRejectsInsubstantialQueries(t *testing.T) {
	g := newGapLog(100)
	now := time.Now()

	g.record("test it and fic it", 0.641, 0.653, true, 3, now)
	g.record("do it now", 0.640, 0.653, true, 3, now)
	g.record("kubernetes ingress nginx annotations", 0.621, 0.653, true, 3, now)

	got := g.Report(1, 0)
	if len(got) != 1 {
		t.Fatalf("only the substantive question belongs on the list, got %d: %+v", len(got), got)
	}

	if got[0].Query != "kubernetes ingress nginx annotations" {
		t.Errorf("wrong survivor: %q", got[0].Query)
	}
}

// TestGapResolveClosesTheLoop: the near-miss prompt used to offer two options and no way to submit
// either — /feedback requires a decision_id and an abstention has no decision. A question with no
// channel to receive its answer trains the reader to ignore the prompt.
func TestGapResolveClosesTheLoop(t *testing.T) {
	g := newGapLog(100)
	now := time.Now()

	const q = "terraform state locking backend"

	g.record(q, 0.62, 0.65, true, 3, now)

	// "no — correctly silent" drops it, so the same non-question stops being asked about.
	if !g.resolve(q, false, now) {
		t.Fatal("resolve should find the entry")
	}

	if len(g.Report(1, 0)) != 0 {
		t.Error("a correctly-silent verdict must remove the entry")
	}

	// ...and it STAYS silenced: re-asking must not re-record it, or the answer meant nothing.
	g.record(q, 0.62, 0.65, true, 3, now)

	if len(g.Report(1, 0)) != 0 {
		t.Error("a silenced question must not come back on the next identical query")
	}

	// "yes — we have a rule" overrides the earlier "no" and promotes it above statistical evidence.
	g.record("some other question entirely here", 0.62, 0.65, true, 3, now)
	g.record("some other question entirely here", 0.62, 0.65, true, 3, now) // seen twice

	if !g.resolve(q, true, now) {
		t.Fatal("a later yes must be able to override an earlier correctly-silent verdict")
	}

	got := g.Report(1, 0)
	if got[0].Query != q || !got[0].Confirmed {
		t.Errorf("a human-confirmed gap must outrank a more-frequent unconfirmed one, got %+v", got[0])
	}

	// Unknown queries are reported honestly rather than silently succeeding.
	if g.resolve("never asked about this", true, now) {
		t.Error("resolving an unknown query must return false")
	}
}

// TestGapLogNeverEvictsConfirmed: Report() promises a human-confirmed gap "outranks any amount of
// statistical evidence", but eviction ranked purely on sighting count — so a resurrected
// yes-after-no entry (Count 1) was silently dropped by a handful of anonymous one-off near-misses,
// which any unauthenticated GET /recall can manufacture. The one entry a person explicitly promoted
// is the one the bound must protect.
func TestGapLogNeverEvictsConfirmed(t *testing.T) {
	g := newGapLog(3)
	now := time.Now()

	const kept = "terraform remote state locking backend"

	g.record(kept, 0.62, 0.65, true, 3, now)
	g.resolve(kept, true, now)

	for _, q := range []string{
		"kubernetes ingress nginx annotations", "postgres connection pool sizing",
		"redis cluster failover behaviour", "grpc deadline propagation rules",
		"otel span naming conventions", "envoy retry budget configuration",
	} {
		g.record(q, 0.62, 0.65, true, 3, now)
	}

	for _, e := range g.Report(1, 0) {
		if e.Query == kept && e.Confirmed {
			return
		}
	}

	t.Error("a human-confirmed gap was evicted by anonymous churn")
}
