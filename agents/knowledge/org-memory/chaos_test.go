package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// flakyEmbedder fails on demand — the dependency this service cannot run without.
type flakyEmbedder struct {
	inner Embedder
	fail  atomic.Bool
	calls atomic.Int64
}

func (f *flakyEmbedder) Embed(ctx context.Context, text string, role EmbedRole) ([]float32, error) {
	f.calls.Add(1)

	if f.fail.Load() {
		return nil, errors.New("embedding provider is down")
	}

	return f.inner.Embed(ctx, text, role)
}

// CHAOS 1 — the embedder dies. The design claims "no silent fallback: a down provider fails loudly
// rather than corrupting the store with mismatched vectors." That is a testable claim and has never
// been tested. The failure mode it guards against is the nasty one: a fallback embedder would write
// vectors from a DIFFERENT space into the same store, silently poisoning every future cosine.
func TestChaos_EmbedderDownFailsLoudly(t *testing.T) {
	fe := &flakyEmbedder{inner: newFakeEmbedder()}

	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.05")

	en := NewEngine(newMemStore(), fe, c)
	ctx := context.Background()

	good, err := en.Capture(ctx, Decision{What: "retry with exponential backoff"})
	if err != nil {
		t.Fatal(err)
	}

	before := len(en.store.Active())

	fe.fail.Store(true)

	if _, err := en.Capture(ctx, Decision{What: "this must not be stored"}); err == nil {
		t.Error("capture must FAIL when the embedder is down, not store an unembedded decision")
	}

	if got := len(en.store.Active()); got != before {
		t.Errorf("a failed capture must not mutate the store: %d -> %d", before, got)
	}

	if _, err := en.Recall(ctx, []string{"retry backoff"}); err == nil {
		t.Error("recall must FAIL loudly rather than silently returning nothing — an empty result is a MEANINGFUL output here, and must never stand in for an outage")
	}

	// ...and it recovers cleanly.
	fe.fail.Store(false)

	out, err := en.Recall(ctx, []string{"retry backoff"})
	if err != nil {
		t.Fatalf("recall should recover: %v", err)
	}

	if !containsID(out, good.ID) {
		t.Error("the pre-outage decision should surface again after recovery")
	}
}

// CHAOS 2 — concurrent everything. Recall, capture, feedback and supersede hammered in parallel
// against one engine. The race detector proves absence of data races; this proves the invariants
// survive interleaving.
func TestChaos_ConcurrentMutationAndRecall(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	seed := make([]string, 0, 20)

	for i := 0; i < 20; i++ {
		d, err := en.Capture(ctx, Decision{
			What:  fmt.Sprintf("retry policy variant %d for downstream calls", i),
			Scope: []string{"repo:x"},
		})
		if err != nil {
			t.Fatal(err)
		}

		seed = append(seed, d.ID)
	}

	var wg sync.WaitGroup

	var recallErrs atomic.Int64

	for w := 0; w < 8; w++ {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for i := 0; i < 40; i++ {
				switch (w + i) % 4 {
				case 0:
					if _, err := en.Recall(ctx, []string{"repo:x", "retry policy downstream"}); err != nil {
						recallErrs.Add(1)
					}
				case 1:
					_, _ = en.Capture(ctx, Decision{What: fmt.Sprintf("new rule %d-%d", w, i)})
				case 2:
					_ = en.RecordFeedback(Feedback{DecisionID: seed[i%len(seed)], Signal: "not_relevant"})
				case 3:
					_, _, _ = en.RecallWithDiag(ctx, []string{"repo:x", "graceful shutdown"})
				}
			}
		}(w)
	}

	wg.Wait()

	if n := recallErrs.Load(); n != 0 {
		t.Errorf("%d recalls errored under concurrency", n)
	}

	// The store must still be coherent: every seeded decision retrievable, no duplicates.
	seen := map[string]int{}
	for _, d := range en.store.Active() {
		seen[d.ID]++
		if seen[d.ID] > 1 {
			t.Fatalf("duplicate decision in Active(): %s", d.ID)
		}
	}

	for _, id := range seed {
		if _, ok := en.store.Get(id); !ok {
			t.Errorf("seeded decision %s vanished under concurrent load", id)
		}
	}
}
