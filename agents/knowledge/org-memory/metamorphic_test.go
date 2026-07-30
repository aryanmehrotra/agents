package main

import (
	"context"
	"fmt"
	"testing"
)

// Metamorphic tests: instead of asserting WHAT the ranker returns (which needs an oracle nobody has),
// assert RELATIONS that must hold between related inputs. These catch bugs no golden set can, because
// they need no notion of the correct answer.

func seeded(t *testing.T) (*Engine, context.Context) {
	t.Helper()

	en := newTestEngine()
	ctx := context.Background()

	for _, w := range []string{
		"retry failed calls with exponential backoff and jitter",
		"wrap downstream dependencies in a circuit breaker",
		"drain connections on graceful shutdown before exit",
		"rate limit requests with a token bucket per tenant",
		"money must be stored as int64 cents never float",
		"log structured json with a correlation id",
		"propagate context deadlines across service calls",
	} {
		if _, err := en.Capture(ctx, Decision{What: w, Scope: []string{"repo:x"}}); err != nil {
			t.Fatal(err)
		}
	}

	return en, ctx
}

func ids(items []RecalledItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Decision.ID
	}

	return out
}

// M1 — DETERMINISM. Same query, same corpus, same answer. Anything else means hidden state.
func TestMeta_Deterministic(t *testing.T) {
	en, ctx := seeded(t)
	q := []string{"repo:x", "retry backoff"}

	first, _ := en.Recall(ctx, q)
	for i := 0; i < 20; i++ {
		got, _ := en.Recall(ctx, q)
		if fmt.Sprint(ids(got)) != fmt.Sprint(ids(first)) {
			t.Fatalf("run %d differed: %v vs %v", i, ids(got), ids(first))
		}
	}
}

// M2 — INSERTION INDEPENDENCE. Adding a wholly unrelated decision must not reorder existing results.
// This is where the fitted noise model could bite: it is estimated over the whole candidate set, so a
// careless implementation lets an irrelevant document shift the gate for everyone.
func TestMeta_UnrelatedInsertionDoesNotReorder(t *testing.T) {
	en, ctx := seeded(t)
	q := []string{"repo:x", "retry backoff"}

	before, _ := en.Recall(ctx, q)

	for i := 0; i < 5; i++ {
		if _, err := en.Capture(ctx, Decision{
			What: fmt.Sprintf("botanical taxonomy of ferns volume %d", i), Scope: []string{"repo:y"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	after, _ := en.Recall(ctx, q)
	if fmt.Sprint(ids(before)) != fmt.Sprint(ids(after)) {
		t.Errorf("unrelated inserts reordered results:\n  before %v\n  after  %v", ids(before), ids(after))
	}
}

// M3 — FLOOR MONOTONICITY. Raising the gate can only ever REMOVE results. If a higher floor ever
// admits something a lower one rejected, the gate is not a gate.
func TestMeta_FloorIsMonotone(t *testing.T) {
	en, ctx := seeded(t)
	q := []string{"repo:x", "retry backoff jitter"}

	var prev []string

	for i, f := range []string{"0.05", "0.15", "0.25", "0.40", "0.60", "0.95"} {
		en.cfg.Set("retrieve.precision_floor", f)

		got, _ := en.Recall(ctx, q)
		cur := ids(got)

		if i > 0 {
			set := map[string]bool{}
			for _, id := range prev {
				set[id] = true
			}

			for _, id := range cur {
				if !set[id] {
					t.Fatalf("floor %s admitted %q that a LOWER floor rejected", f, id)
				}
			}
		}

		prev = cur
	}
}

// M4 — TOP_K IS A PREFIX. top_k=5 must begin with exactly what top_k=3 returned; a deeper request
// must not reshuffle the shallow one.
func TestMeta_TopKIsAPrefix(t *testing.T) {
	en, ctx := seeded(t)
	en.cfg.Set("retrieve.precision_floor", "0.05")

	q := []string{"repo:x", "retry backoff"}

	en.cfg.Set("retrieve.top_k", "2")
	shallow, _ := en.Recall(ctx, q)

	en.cfg.Set("retrieve.top_k", "6")
	deep, _ := en.Recall(ctx, q)

	if len(deep) < len(shallow) {
		t.Fatalf("deeper request returned fewer: %d < %d", len(deep), len(shallow))
	}

	for i, id := range ids(shallow) {
		if deep[i].Decision.ID != id {
			t.Fatalf("top_k=6 reshuffled the top_k=2 prefix at %d: %v vs %v", i, ids(deep), ids(shallow))
		}
	}
}

// M5 — SUPERSEDE ONLY REMOVES. Retiring a decision must drop it and must not promote anything that
// was previously gated out; the survivors' relative order must hold.
func TestMeta_SupersedeOnlyRemoves(t *testing.T) {
	en := newTestEngine()
	en.cfg.Set("retrieve.precision_floor", "0.001")
	en.cfg.Set("retrieve.top_k", "6")

	ctx := context.Background()

	// A corpus that deliberately shares a term, so several decisions clear the gate for one query and
	// there is a real ordering to preserve.
	for i, w := range []string{
		"retry policy alpha for downstream calls",
		"retry policy beta for downstream calls",
		"retry policy gamma for downstream calls",
		"retry policy delta for downstream calls",
	} {
		if _, err := en.Capture(ctx, Decision{What: w, Scope: []string{fmt.Sprintf("repo:r%d", i)}}); err != nil {
			t.Fatal(err)
		}
	}

	q := []string{"retry policy downstream calls"}

	before, _ := en.Recall(ctx, q)
	if len(before) < 3 {
		t.Fatalf("precondition: need 3+ results, got %d", len(before))
	}

	victim := before[1].Decision.ID

	replacement, _ := en.Capture(ctx, Decision{What: "a totally unrelated rule about typography"})
	if err := en.Supersede(victim, replacement.ID); err != nil {
		t.Fatal(err)
	}

	after, _ := en.Recall(ctx, q)

	for _, it := range after {
		if it.Decision.ID == victim {
			t.Fatal("a superseded decision still surfaced")
		}
	}

	var wantOrder []string

	for _, id := range ids(before) {
		if id != victim {
			wantOrder = append(wantOrder, id)
		}
	}

	got := ids(after)
	for i := 0; i < len(got) && i < len(wantOrder); i++ {
		if got[i] != wantOrder[i] {
			t.Errorf("supersede reordered the survivors:\n  want %v\n  got  %v", wantOrder, got)
			break
		}
	}
}
