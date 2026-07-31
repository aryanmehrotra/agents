package main

import "testing"

func TestMemStoreSupersedeNeverDeletes(t *testing.T) {
	s := newMemStore()
	s.Put(Decision{ID: "d-old", What: "use REST"})
	s.Put(Decision{ID: "d-new", What: "use gRPC", Supersedes: "d-old"})

	if !s.Supersede("d-old", "d-new") {
		t.Fatal("supersede of an existing id should succeed")
	}

	got, ok := s.Get("d-old")
	if !ok {
		t.Fatal("INVARIANT: superseded row must be kept, never deleted")
	}

	if got.SupersededBy != "d-new" {
		t.Fatalf("supersede link must be set on the old row: %+v", got)
	}

	for _, d := range s.Active() {
		if d.ID == "d-old" {
			t.Fatal("superseded row must NOT be an active recall candidate")
		}
	}

	if a := s.Active(); len(a) != 1 || a[0].ID != "d-new" {
		t.Fatalf("active should be just d-new, got %+v", a)
	}

	if s.Supersede("nope", "d-new") {
		t.Fatal("superseding an unknown id should return false")
	}
}

func TestMemStoreQuarantineKeepsRow(t *testing.T) {
	s := newMemStore()
	s.Put(Decision{ID: "d-1", What: "x"})

	if !s.Quarantine("d-1") {
		t.Fatal("quarantine should succeed")
	}

	got, ok := s.Get("d-1")
	if !ok || !got.Quarantined {
		t.Fatal("quarantine must set the flag and keep the row")
	}

	if s.Quarantine("nope") {
		t.Fatal("quarantining unknown id should return false")
	}
}

func TestMemStoreFeedbackCounters(t *testing.T) {
	s := newMemStore()
	s.Bump("d-1", "helpful", 2)
	s.Bump("d-1", "used", 1)
	s.Bump("d-1", "not_relevant", 1)
	s.Bump("d-1", "wrong", 3)
	s.Bump("d-1", "outdated", 1) // folds into wrong

	st := s.Stats("d-1")
	if st.Helpful != 2 || st.Used != 1 || st.NotRelevant != 1 || st.Wrong != 4 {
		t.Fatalf("counter math wrong: %+v", st)
	}
}
