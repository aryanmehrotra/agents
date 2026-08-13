package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteStoreInvariants(t *testing.T) {
	s, err := newSQLiteStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Put(Decision{ID: "d-old", What: "use REST", Scope: []string{"service:pay"}, Embedding: []float32{1, 0, 0}})
	s.Put(Decision{ID: "d-new", What: "use gRPC", Scope: []string{"service:pay"}})

	if !s.Supersede("d-old", "d-new") {
		t.Fatal("supersede of existing id should be true")
	}

	if s.Supersede("nope", "x") {
		t.Fatal("supersede of unknown id should be false")
	}

	got, ok := s.Get("d-old")
	if !ok {
		t.Fatal("INVARIANT: superseded row must be kept")
	}

	if got.SupersededBy != "d-new" {
		t.Fatalf("supersede link must persist: %+v", got)
	}

	if len(got.Embedding) != 3 || got.Embedding[0] != 1 {
		t.Fatalf("embedding must round-trip through the blob: %+v", got.Embedding)
	}

	for _, d := range s.Active() {
		if d.ID == "d-old" {
			t.Fatal("superseded row must not be active")
		}
	}

	if a := s.Active(); len(a) != 1 || a[0].ID != "d-new" {
		t.Fatalf("active should be just d-new, got %+v", a)
	}

	if !s.Quarantine("d-new") {
		t.Fatal("quarantine should be true")
	}

	if q, _ := s.Get("d-new"); !q.Quarantined {
		t.Fatal("quarantine flag must be kept")
	}

	s.Bump("d-new", "helpful", 2)
	s.Bump("d-new", "wrong", 1)
	s.Bump("d-new", "outdated", 1) // folds into wrong

	if st := s.Stats("d-new"); st.Helpful != 2 || st.Wrong != 2 {
		t.Fatalf("counter math wrong: %+v", st)
	}
}

func TestSQLiteEnginePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	ctx := context.Background()

	cfg := func() *Config {
		c := NewConfig()
		c.Set("retrieve.precision_floor", "0.15")

		return c
	}

	s1, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}

	en1 := NewEngine(s1, newFakeEmbedder(), cfg())
	d, _ := en1.Capture(ctx, Decision{
		What: "money must be stored as int64 cents", Why: "float caused an incident", Scope: []string{"service:pay"},
	})
	_ = en1.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "helpful"})
	_ = s1.Close()

	// reopen the same DB file — everything must still be there and recall must work.
	s2, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	en2 := NewEngine(s2, newFakeEmbedder(), cfg())

	if _, ok := s2.Get(d.ID); !ok {
		t.Fatal("decision must persist across reopen")
	}

	if s2.Stats(d.ID).Helpful != 1 {
		t.Fatal("feedback must persist across reopen")
	}

	out, _ := en2.Recall(ctx, []string{"service:pay", "handling money amounts in cents"})
	if len(out) == 0 || out[0].Decision.ID != d.ID {
		t.Fatalf("recall must work after reopen, got %+v", out)
	}
}
