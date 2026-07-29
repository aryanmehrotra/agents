package main

import (
	"context"
	"testing"
	"time"
)

func containsID(items []RecalledItem, id string) bool {
	for _, it := range items {
		if it.Decision.ID == id {
			return true
		}
	}

	return false
}

// Retrievability must DECAY with disuse: an old, never-reinforced decision is less retrievable than
// a fresh one (Bjork & Bjork 1992 — retrieval strength falls with disuse).
func TestRetentionDecaysWithDisuse(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()

	fresh := Decision{Updated: now}
	stale := Decision{Updated: now.AddDate(0, 0, -400)} // >4 half-lives old

	rFresh := retention(fresh, Stats{}, now, cfg)
	rStale := retention(stale, Stats{}, now, cfg)

	if !(rFresh > rStale) {
		t.Fatalf("fresh (%.3f) must be more retrievable than stale (%.3f)", rFresh, rStale)
	}

	if rStale >= cfg.F("forget.floor", 0.03) {
		t.Fatalf("a 400-day unused decision should fall below the forget floor; got %.3f", rStale)
	}
}

// Use REINFORCES: the same stale decision, but marked helpful, must be MORE retrievable than the
// untouched stale one — and survive above the floor (value-directed remembering / synaptic tagging).
func TestRetentionReinforcedByUse(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()

	stale := Decision{Updated: now.AddDate(0, 0, -400)}

	rCold := retention(stale, Stats{}, now, cfg)
	rWarm := retention(stale, Stats{Helpful: 3, Used: 2}, now, cfg)

	if !(rWarm > rCold) {
		t.Fatalf("reinforcement must raise retrievability: warm %.3f should exceed cold %.3f", rWarm, rCold)
	}

	if rWarm < cfg.F("forget.floor", 0.03) {
		t.Fatalf("a reinforced decision must survive above the forget floor; got %.3f", rWarm)
	}
}

// Negative feedback ERODES retrievability below an equivalent neutral decision.
func TestRetentionErodedByNegative(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()

	d := Decision{Updated: now.AddDate(0, 0, -30)}

	rNeutral := retention(d, Stats{}, now, cfg)
	rBad := retention(d, Stats{Wrong: 3, NotRelevant: 2}, now, cfg)

	if !(rBad < rNeutral) {
		t.Fatalf("negative feedback must lower retrievability: bad %.3f should be under neutral %.3f", rBad, rNeutral)
	}
}

// The end-to-end forgetting guarantee: a decayed decision is DEMOTED out of recall (not surfaced),
// yet REMAINS in the store (Get returns it) — forgetting is demote-not-delete, and it can revive.
func TestForgetFloorDemotesButKeeps(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	// strong lexical overlap so the deterministic test embedder ranks it well above the floor —
	// this isolates the variable under test (retention), not similarity.
	query := []string{"repo:x", "retry backoff exponential jitter"}

	d, err := en.Capture(ctx, Decision{What: "retry backoff exponential jitter", Why: "avoid thundering herd", Scope: []string{"repo:x"}})
	if err != nil {
		t.Fatal(err)
	}

	// FRESH: it must surface (proves demotion later is about retention, not relevance)
	fresh, err := en.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	if !containsID(fresh, d.ID) {
		t.Fatalf("precondition: a fresh, relevant decision should surface; got %+v", fresh)
	}

	// age it past its half-life with no reinforcement → it should fall below the forget floor
	d.Updated = time.Now().AddDate(0, 0, -400)
	en.store.Put(d)

	out, err := en.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	if containsID(out, d.ID) {
		t.Fatalf("a decayed decision must be demoted from recall, but it was surfaced")
	}

	// ...yet it is NOT deleted — still fully present in the store (cold storage).
	if _, ok := en.store.Get(d.ID); !ok {
		t.Fatal("forgetting must DEMOTE, never delete — the decision vanished from the store")
	}

	// reinforcing it revives retrievability above the floor (retrieval strength jumps on use)
	en.store.Bump(d.ID, "helpful", 3)

	if r := retention(d, en.store.Stats(d.ID), time.Now(), en.cfg); r < en.cfg.F("forget.floor", 0.03) {
		t.Fatalf("reinforcement should revive a demoted decision above the floor; got %.3f", r)
	}
}

// Consolidate reports hot vs cold honestly and MUTATES NOTHING (introspection only).
func TestConsolidateReportsWithoutMutating(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	keep, _ := en.Capture(ctx, Decision{What: "keep: fresh decision", Scope: []string{"repo:x"}})
	fade, _ := en.Capture(ctx, Decision{What: "fade: old unused decision", Scope: []string{"repo:x"}})

	fade.Updated = time.Now().AddDate(0, 0, -500)
	en.store.Put(fade)

	before := len(en.store.Active())

	rep := en.Consolidate(time.Now(), 10)

	if rep.Total != 2 || rep.Hot < 1 || rep.Cold < 1 {
		t.Fatalf("expected 1 hot + 1 cold of 2; got total=%d hot=%d cold=%d", rep.Total, rep.Hot, rep.Cold)
	}

	if len(rep.Coldest) == 0 || rep.Coldest[0].ID != fade.ID {
		t.Fatalf("the coldest item should be the aged 'fade' decision; got %+v", rep.Coldest)
	}

	if _, ok := en.store.Get(keep.ID); !ok {
		t.Fatal("consolidation must not drop the hot decision")
	}

	if after := len(en.store.Active()); after != before {
		t.Fatalf("Consolidate must not mutate the store: active %d → %d", before, after)
	}
}
