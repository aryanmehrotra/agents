package main

import (
	"context"
	"strings"
	"testing"
)

func newTestEngine() *Engine {
	c := NewConfig()
	// bag-of-words cosine: related text lands ~0.28-0.8, truly-unrelated ~0. A 0.15 floor cleanly
	// separates "relevant" from "inject nothing".
	c.Set("retrieve.precision_floor", "0.15")

	return NewEngine(newMemStore(), newFakeEmbedder(), c)
}

func TestCaptureDedupeAndReinforce(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	d1, err := en.Capture(ctx, Decision{What: "money is int64 cents", Scope: []string{"service:pay"}})
	if err != nil {
		t.Fatal(err)
	}

	d2, err := en.Capture(ctx, Decision{
		What: "  Money is int64 cents ", Scope: []string{"service:pay"}, Why: "float caused an incident",
	})
	if err != nil {
		t.Fatal(err)
	}

	if d1.ID != d2.ID {
		t.Fatalf("same knowledge should dedupe to one id: %s vs %s", d1.ID, d2.ID)
	}

	if d2.Why == "" {
		t.Fatal("reinforce should enrich the previously-empty why")
	}

	if got := len(en.store.Active()); got != 1 {
		t.Fatalf("dedupe should keep a single active decision, got %d", got)
	}
}

func TestCaptureEmptyWhatRejected(t *testing.T) {
	en := newTestEngine()
	if _, err := en.Capture(context.Background(), Decision{What: "   "}); err == nil {
		t.Fatal("empty `what` must be rejected")
	}
}

func TestRecallSurfacesRelevantWithinScope(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	_, _ = en.Capture(ctx, Decision{
		What: "money must be stored as int64 cents never float",
		Why:  "float rounding caused an incident", Scope: []string{"service:pay"},
	})
	_, _ = en.Capture(ctx, Decision{
		What: "retry external calls with exponential backoff",
		Why:  "avoid thundering herd", Scope: []string{"service:pay"},
	})
	_, _ = en.Capture(ctx, Decision{What: "use flexbox for the sidebar layout", Scope: []string{"service:web"}})

	out, err := en.Recall(ctx, []string{"service:pay", "handling money amounts in cents"})
	if err != nil {
		t.Fatal(err)
	}

	if len(out) == 0 {
		t.Fatal("expected the money decision to surface")
	}

	if !strings.HasPrefix(out[0].Decision.What, "money") {
		t.Fatalf("most relevant should be the money decision, got %q", out[0].Decision.What)
	}

	for _, it := range out {
		if len(it.Decision.Scope) > 0 && it.Decision.Scope[0] == "service:web" {
			t.Fatal("out-of-scope (service:web) decision must not surface for a service:pay context")
		}
	}
}

func TestRecallInjectsNothingWhenIrrelevant(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	_, _ = en.Capture(ctx, Decision{What: "money must be stored as int64 cents", Scope: []string{"service:pay"}})

	out, _ := en.Recall(ctx, []string{"service:pay", "kubernetes pod dns networking config"})
	if len(out) != 0 {
		t.Fatalf("no relevant decision → inject NOTHING, got %+v", out)
	}
}

func TestFeedbackWrongQuarantinesAndStopsSurfacing(t *testing.T) {
	en := newTestEngine()
	en.cfg.Set("feedback.wrong_quarantine_at", "2")
	ctx := context.Background()

	d, _ := en.Capture(ctx, Decision{What: "always disable auth in dev builds", Scope: []string{"service:pay"}})
	q := []string{"service:pay", "auth in dev builds"}

	if out, _ := en.Recall(ctx, q); len(out) == 0 {
		t.Fatal("precondition: decision should surface before any feedback")
	}

	_ = en.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "wrong"})
	_ = en.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "wrong"})

	if out, _ := en.Recall(ctx, q); len(out) != 0 {
		t.Fatalf("after 2x wrong the decision must be quarantined and not surface, got %+v", out)
	}
}

func TestFeedbackValidation(t *testing.T) {
	en := newTestEngine()
	d, _ := en.Capture(context.Background(), Decision{What: "x", Scope: []string{"s"}})

	if err := en.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "banana"}); err == nil {
		t.Fatal("unknown feedback signal must be rejected")
	}

	if err := en.RecordFeedback(Feedback{Signal: "helpful"}); err == nil {
		t.Fatal("feedback without decision_id must be rejected")
	}
}

func TestCaptureSupersedeNeverLosesAndSwaps(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	old, _ := en.Capture(ctx, Decision{What: "use REST for internal rpc calls", Scope: []string{"service:pay"}})
	_, _ = en.Capture(ctx, Decision{
		What: "use gRPC for internal rpc calls", Scope: []string{"service:pay"}, Supersedes: old.ID,
	})

	if _, ok := en.store.Get(old.ID); !ok {
		t.Fatal("superseded decision must be kept (never deleted)")
	}

	out, _ := en.Recall(ctx, []string{"service:pay", "internal rpc calls protocol"})

	for _, it := range out {
		if it.Decision.ID == old.ID {
			t.Fatal("superseded (REST) decision must not surface")
		}
	}

	if len(out) == 0 || !strings.HasPrefix(out[0].Decision.What, "use gRPC") {
		t.Fatalf("new (gRPC) decision should surface, got %+v", out)
	}
}
