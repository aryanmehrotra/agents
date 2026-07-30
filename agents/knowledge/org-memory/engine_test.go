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

// TestRecallDiagMeasuresAbstention guards the honesty fix: an abstention must report how close it
// came. Taking the top similarity of an already-empty result list yields 0.0, and a validation report
// then reads that 0.0 as a measured cosine and concludes the abstention margin is "enormous". It is
// not a cosine — it is an absence. The pre-gate best must be carried out separately and be real.
func TestRecallDiagMeasuresAbstention(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	if _, err := en.Capture(ctx, Decision{
		What: "money must be stored as int64 cents never float", Scope: []string{"service:pay"},
	}); err != nil {
		t.Fatal(err)
	}

	// Related enough to score above zero, nowhere near enough to clear the floor.
	en.cfg.Set("retrieve.precision_floor", "0.95")

	items, diag, err := en.RecallWithDiag(ctx, []string{"money stored as cents"})
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 0 {
		t.Fatalf("nothing should clear a 0.95 floor, got %d items", len(items))
	}

	if diag.TopSimilarity <= 0 {
		t.Fatal("an abstention must still report the best similarity actually achieved, not 0")
	}

	if diag.TopSimilarity >= diag.Floor {
		t.Fatalf("the near-miss must sit below the floor it failed: top=%.4f floor=%.4f",
			diag.TopSimilarity, diag.Floor)
	}

	if diag.Candidates != 1 {
		t.Fatalf("expected 1 scored candidate, got %d", diag.Candidates)
	}
}

// TestCaptureSupersedeOnReinforcePath is the regression guard for a silent no-op. Capture's dedupe
// branch returned BEFORE the supersede branch, so re-capturing an already-known decision with
// `supersedes` set retired nothing — and reported success. Content-addressed ids make this the
// common path, not an edge case: restating a known rule in order to retire an older one hashes to
// the existing id and lands right here.
func TestCaptureSupersedeOnReinforcePath(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	stale, err := en.Capture(ctx, Decision{What: "deploy on fridays is fine"})
	if err != nil {
		t.Fatal(err)
	}

	current, err := en.Capture(ctx, Decision{What: "never deploy on fridays"})
	if err != nil {
		t.Fatal(err)
	}

	// Re-capture the SAME what+scope (⇒ same id ⇒ dedupe/reinforce branch), now asking it to retire
	// the stale decision. This is the call that used to be silently ignored.
	again, err := en.Capture(ctx, Decision{What: "never deploy on fridays", Supersedes: stale.ID})
	if err != nil {
		t.Fatal(err)
	}

	if again.ID != current.ID {
		t.Fatalf("precondition: identical what+scope must dedupe to one id, got %s vs %s", again.ID, current.ID)
	}

	retired, ok := en.store.Get(stale.ID)
	if !ok {
		t.Fatal("supersede must never delete")
	}

	if retired.SupersededBy != current.ID {
		t.Fatalf("supersede was silently dropped on the reinforce path: superseded_by=%q", retired.SupersededBy)
	}

	for _, d := range en.store.Active() {
		if d.ID == stale.ID {
			t.Fatal("the retired decision must stop being an active recall candidate")
		}
	}
}

// TestCaptureSelfSupersedeRefused: a decision that supersedes itself would set SupersededBy on its
// own row and vanish from recall entirely — a delete, in a system that promises never to delete.
func TestCaptureSelfSupersedeRefused(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	d, _ := en.Capture(ctx, Decision{What: "prefer table driven tests"})

	again, err := en.Capture(ctx, Decision{What: "prefer table driven tests", Supersedes: d.ID})
	if err != nil {
		t.Fatal(err)
	}

	if again.SupersededBy != "" {
		t.Fatalf("self-supersede must be refused, got superseded_by=%q", again.SupersededBy)
	}

	if len(en.store.Active()) != 1 {
		t.Fatalf("the decision must remain active, got %d active", len(en.store.Active()))
	}
}

// TestSupersedeRetroactively covers the verb that was missing: two decisions already in the store,
// discovered later to conflict, reconciled without deleting either.
func TestSupersedeRetroactively(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	stale, err := en.Capture(ctx, Decision{What: "clickhouse is the sole store for cost records"})
	if err != nil {
		t.Fatal(err)
	}

	current, err := en.Capture(ctx, Decision{What: "dual write cost records to mysql and clickhouse"})
	if err != nil {
		t.Fatal(err)
	}

	if err := en.Supersede(stale.ID, current.ID); err != nil {
		t.Fatalf("retroactive supersede should succeed: %v", err)
	}

	// Retired: gone from recall candidates, but NOT gone.
	for _, d := range en.store.Active() {
		if d.ID == stale.ID {
			t.Fatal("a superseded decision must stop being an active recall candidate")
		}
	}

	kept, ok := en.store.Get(stale.ID)
	if !ok {
		t.Fatal("supersede must never delete — the retired row is kept in full")
	}

	if kept.SupersededBy != current.ID {
		t.Fatalf("retired row should link forward to its replacement, got %q", kept.SupersededBy)
	}

	if back, _ := en.store.Get(current.ID); back.Supersedes != stale.ID {
		t.Fatalf("replacement should link back to what it retired, got %q", back.Supersedes)
	}
}

// TestSupersedeRejectsRetiredTarget: linking into an already-retired decision would hide the newly
// retired one behind a head recall never surfaces — a silent delete in a never-delete system.
func TestSupersedeRejectsRetiredTarget(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	a, _ := en.Capture(ctx, Decision{What: "first rule about timeouts"})
	b, _ := en.Capture(ctx, Decision{What: "second rule about timeouts"})
	c, _ := en.Capture(ctx, Decision{What: "third rule about timeouts"})

	if err := en.Supersede(b.ID, c.ID); err != nil {
		t.Fatal(err)
	}

	if err := en.Supersede(a.ID, b.ID); err == nil {
		t.Fatal("superseding into an already-retired decision must be refused")
	}

	if err := en.Supersede(a.ID, a.ID); err == nil {
		t.Fatal("a decision must not supersede itself")
	}

	if err := en.Supersede(a.ID, "d-does-not-exist"); err == nil {
		t.Fatal("an unknown replacement must be refused")
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

// TestNotRelevantIsScopedToTheQuestion completes the relation-vs-property fix.
//
// `not_relevant` says "this was a poor answer to what I ASKED" — a claim about a query↔decision pair,
// not about the decision (Saracevic, JASIST 2007). Stored as a lifetime total it suppressed the
// decision for every OTHER question too, which is a category error with real consequences: one bad
// match could bury a correct rule permanently, everywhere.
func TestNotRelevantIsScopedToTheQuestion(t *testing.T) {
	en := newTestEngine()
	en.cfg.Set("feedback.demote_per_notrel", "0.5") // large, so a demotion is unmistakable
	ctx := context.Background()

	d, err := en.Capture(ctx, Decision{
		What: "money must be stored as int64 cents never float", Scope: []string{"service:pay"},
	})
	if err != nil {
		t.Fatal(err)
	}

	const asked = "money stored as int64 cents"

	const other = "handling currency rounding in reports"

	// Surface it for one question, then say it was a bad answer TO THAT QUESTION.
	if _, err := en.Recall(ctx, []string{asked}); err != nil {
		t.Fatal(err)
	}

	if err := en.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "not_relevant"}); err != nil {
		t.Fatal(err)
	}

	if got := en.scopedNotRelevant(d.ID, asked); got != 1 {
		t.Fatalf("the judgement must attach to the question it was made about, got %d", got)
	}

	if got := en.scopedNotRelevant(d.ID, other); got != 0 {
		t.Errorf("a different question must be unaffected, got %d — the demotion travelled", got)
	}

	// The lifetime total still moves, for reporting.
	if got := en.store.Stats(d.ID).NotRelevant; got != 1 {
		t.Errorf("lifetime total should still count it for /stats, got %d", got)
	}

	// And ranking sees the scoped value, not the lifetime one.
	if st := en.scopedStats(d.ID, other); st.NotRelevant != 0 {
		t.Errorf("ranking for an unrelated question must see no demotion, got %d", st.NotRelevant)
	}

	if st := en.scopedStats(d.ID, asked); st.NotRelevant != 1 {
		t.Errorf("ranking for the SAME question must still see the demotion, got %d", st.NotRelevant)
	}
}

// TestNotRelevantHonoursExplicitContext: Feedback.Context has existed since Phase 0 and was never
// read. It is the caller's statement of which information need the judgement belongs to, and it must
// win over the engine's guess.
func TestNotRelevantHonoursExplicitContext(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	d, _ := en.Capture(ctx, Decision{What: "prefer table driven tests"})

	err := en.RecordFeedback(Feedback{
		DecisionID: d.ID, Signal: "not_relevant",
		Context: []string{"how should I structure Go tests"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := en.scopedNotRelevant(d.ID, "how should I structure Go tests"); got != 1 {
		t.Fatalf("explicit context must scope the judgement, got %d", got)
	}

	if got := en.scopedNotRelevant(d.ID, "something entirely different"); got != 0 {
		t.Errorf("it must not leak to other questions, got %d", got)
	}
}

// TestUnattributableNotRelevantDoesNotDemote: a relation with no second term cannot be acted on. It
// is still counted for reporting, but must not silently become a global demotion — which is exactly
// the behaviour being removed.
func TestUnattributableNotRelevantDoesNotDemote(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	d, _ := en.Capture(ctx, Decision{What: "use context for cancellation"})

	// No Context, and never surfaced — so nothing to attribute it to.
	if err := en.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "not_relevant"}); err != nil {
		t.Fatal(err)
	}

	if st := en.scopedStats(d.ID, "any question at all"); st.NotRelevant != 0 {
		t.Errorf("an unattributable judgement must not demote anywhere, got %d", st.NotRelevant)
	}

	if got := en.store.Stats(d.ID).NotRelevant; got != 1 {
		t.Errorf("it should still be counted for reporting, got %d", got)
	}
}
