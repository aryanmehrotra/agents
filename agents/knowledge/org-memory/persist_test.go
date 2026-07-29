package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.json")
	ctx := context.Background()

	// session 1: capture + feedback, then let it fall out of scope (simulating a restart).
	fs1, err := newFileStore(path)
	if err != nil {
		t.Fatal(err)
	}

	c1 := NewConfig()
	c1.Set("retrieve.precision_floor", "0.15")
	en1 := NewEngine(fs1, newFakeEmbedder(), c1)

	d, _ := en1.Capture(ctx, Decision{
		What: "money must be stored as int64 cents", Why: "float caused an incident", Scope: []string{"service:pay"},
	})
	_ = en1.RecordFeedback(Feedback{DecisionID: d.ID, Signal: "helpful"})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file should exist after a write: %v", err)
	}

	// session 2: a fresh fileStore from the same path must see everything, incl. the embedding.
	fs2, err := newFileStore(path)
	if err != nil {
		t.Fatal(err)
	}

	c2 := NewConfig()
	c2.Set("retrieve.precision_floor", "0.15")
	en2 := NewEngine(fs2, newFakeEmbedder(), c2)

	got, ok := fs2.Get(d.ID)
	if !ok {
		t.Fatal("decision must persist across reload")
	}

	if len(got.Embedding) == 0 {
		t.Fatal("embedding must persist across reload (recall depends on it)")
	}

	if fs2.Stats(d.ID).Helpful != 1 {
		t.Fatal("feedback counters must persist across reload")
	}

	out, _ := en2.Recall(ctx, []string{"service:pay", "handling money amounts in cents"})
	if len(out) == 0 || out[0].Decision.ID != d.ID {
		t.Fatalf("recall must still work after reload, got %+v", out)
	}
}
