package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// DIFFERENTIAL testing: the Store interface has three implementations and the engine is supposed not
// to care which. That is a claim, and nothing has ever checked it. Production runs SQLite; every test
// in this repo but one runs the in-memory store — so any behavioural divergence between them is
// invisible to the entire suite and shows up only in production.
func TestDiff_MemStoreAndSQLiteAgree(t *testing.T) {
	dir := t.TempDir()

	sq, err := newSQLiteStore(filepath.Join(dir, "diff.db"))
	if err != nil {
		t.Fatal(err)
	}

	build := func(s Store) *Engine {
		c := NewConfig()
		c.Set("retrieve.precision_floor", "0.05")
		c.Set("retrieve.top_k", "5")

		return NewEngine(s, newFakeEmbedder(), c)
	}

	engines := map[string]*Engine{"mem": build(newMemStore()), "sqlite": build(sq)}
	ctx := context.Background()

	corpus := []Decision{
		{What: "retry failed calls with exponential backoff", Scope: []string{"repo:x"}},
		{What: "wrap downstream calls in a circuit breaker", Scope: []string{"repo:x"}},
		{What: "money is int64 cents never float", Scope: []string{"service:pay"}},
		{What: "drain connections on graceful shutdown", Scope: []string{"repo:x"}},
		{What: "tenant scoped secret rotation policy", Scope: []string{"org:acme"}},
	}

	captured := map[string][]string{}

	for name, en := range engines {
		for _, d := range corpus {
			got, err := en.Capture(ctx, d)
			if err != nil {
				t.Fatalf("%s capture: %v", name, err)
			}

			captured[name] = append(captured[name], got.ID)
		}
	}

	// Content-addressed ids must be identical across backends.
	if fmt.Sprint(captured["mem"]) != fmt.Sprint(captured["sqlite"]) {
		t.Fatalf("ids diverge:\n  mem    %v\n  sqlite %v", captured["mem"], captured["sqlite"])
	}

	// Same recalls, same feedback, same supersede — then compare everything observable.
	for _, en := range engines {
		_, _ = en.Recall(ctx, []string{"repo:x", "retry backoff"})
		_ = en.RecordFeedback(Feedback{DecisionID: captured["mem"][0], Signal: "not_relevant"})
		_ = en.RecordFeedback(Feedback{DecisionID: captured["mem"][1], Signal: "wrong"})
		_ = en.Supersede(captured["mem"][3], captured["mem"][2])
	}

	for _, q := range [][]string{
		{"repo:x", "retry backoff"},
		{"service:pay", "money cents"},
		{"repo:x", "graceful shutdown drain"},
		{"what is the capital of France"},
		{"org:acme", "secret rotation"},
		{"secret rotation"}, // isolation: must be refused by both
	} {
		var want string

		for _, name := range []string{"mem", "sqlite"} {
			out, err := engines[name].Recall(ctx, q)
			if err != nil {
				t.Fatalf("%s recall %v: %v", name, q, err)
			}

			got := fmt.Sprint(ids(out))
			if name == "mem" {
				want = got
				continue
			}

			if got != want {
				t.Errorf("backends disagree on %v:\n  mem    %s\n  sqlite %s", q, want, got)
			}
		}
	}

	// Scoped relevance counters must agree too — the newest feature, and the one with a composite key.
	for _, id := range captured["mem"] {
		m := engines["mem"].scopedNotRelevant(id, "retry backoff")
		s := engines["sqlite"].scopedNotRelevant(id, "retry backoff")

		if m != s {
			t.Errorf("scoped not_relevant diverges for %s: mem=%d sqlite=%d", id, m, s)
		}
	}

	// And the durable one must survive a reopen with a completely fresh engine.
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sq2, err := newSQLiteStore(filepath.Join(dir, "diff.db"))
	if err != nil {
		t.Fatal(err)
	}

	reopened := build(sq2)
	if got := reopened.scopedNotRelevant(captured["mem"][0], "retry backoff"); got != 1 {
		t.Errorf("scoped counter did not persist across reopen: got %d want 1", got)
	}

	if len(reopened.store.Active()) != len(engines["sqlite"].store.Active()) {
		t.Error("active set changed across reopen")
	}
}
