package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentCaptureRecallFeedbackIsConsistent hammers the engine from many goroutines and asserts
// no updates are lost or duplicated — run with -race to also catch data races. This is the Phase-0
// consistency gate.
func TestConcurrentCaptureRecallFeedbackIsConsistent(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	seed, _ := en.Capture(ctx, Decision{What: "seed decision about money cents", Scope: []string{"service:pay"}})

	const n = 50

	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			_, _ = en.Capture(ctx, Decision{
				What: fmt.Sprintf("decision number %d about money handling", i), Scope: []string{"service:pay"},
			})
			_, _ = en.Recall(ctx, []string{"service:pay", "money handling"})
			_ = en.RecordFeedback(Feedback{DecisionID: seed.ID, Signal: "helpful"})
		}(i)
	}

	wg.Wait()

	if got := len(en.store.Active()); got != n+1 {
		t.Fatalf("expected %d active decisions (n unique + seed), got %d — updates lost/duplicated", n+1, got)
	}

	if got := en.store.Stats(seed.ID).Helpful; got != n {
		t.Fatalf("expected exactly %d helpful marks, got %d — a concurrent update was lost", n, got)
	}
}
