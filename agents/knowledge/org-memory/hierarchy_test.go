package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The key property: a query in a CHILD scope also surfaces the PARENT's decisions (inheritance),
// once the tree is confirmed.
func TestHierarchyInheritanceInRecall(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	// parent-level (core/docs) decision — scoped to the root only
	_, _ = en.Capture(ctx, Decision{
		What: "all services expose a health endpoint at /health", Why: "uniform ops", Scope: []string{"repo:acme"},
	})
	// child-level decision
	_, _ = en.Capture(ctx, Decision{
		What: "aggregator batches clickhouse reads for health", Why: "perf", Scope: []string{"repo:acme", "service:aggregator"},
	})

	q := []string{"service:aggregator", "health endpoint"}

	// BEFORE the tree exists, a service-scoped query should NOT pull the root-only decision
	// (its scope repo:acme isn't in the context)... actually repo:acme IS in the query? No —
	// the query is service:aggregator. Confirm inheritance changes the result.
	en.SetParent("service:aggregator", "layer:backend")
	en.SetParent("layer:backend", "repo:acme")

	out, err := en.Recall(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	sawParent := false
	for _, it := range out {
		if strings.HasPrefix(it.Decision.What, "all services") {
			sawParent = true
		}
	}

	if !sawParent {
		t.Fatalf("a child-scope query must INHERIT the parent (repo:acme) decision; got %+v", out)
	}
}

func TestProposeAndConfirmPersist(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	_, _ = en.Capture(ctx, Decision{What: "x", Scope: []string{"repo:acme", "service:agg"}})
	_, _ = en.Capture(ctx, Decision{What: "y", Scope: []string{"repo:acme", "frontend:app"}})

	props := en.Propose()
	if len(props) == 0 {
		t.Fatal("Propose should suggest relations for unlinked scope tags")
	}

	// backend/frontend layers must be proposed as children of the root
	var backendToRoot bool
	for _, r := range props {
		if r.Child == "layer:backend" && r.Parent == "repo:acme" {
			backendToRoot = true
		}
	}

	if !backendToRoot {
		t.Fatalf("layer:backend should be proposed as a child of the root repo; got %+v", props)
	}

	en.ConfirmRelation("service:agg", "layer:backend", true)

	found := false
	for _, r := range en.Hierarchy().Relations {
		if r.Child == "service:agg" && r.Status == "confirmed" {
			found = true
		}
	}

	if !found {
		t.Fatal("a confirmed relation must be stored")
	}
}

func TestSQLiteRelationsPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")

	s1, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}

	s1.SetRelation("service:agg", "layer:backend", "confirmed")
	_ = s1.Close()

	s2, err := newSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	rs := s2.Relations()
	if len(rs) != 1 || rs[0].Child != "service:agg" || rs[0].Parent != "layer:backend" || rs[0].Status != "confirmed" {
		t.Fatalf("relation must persist across reopen: %+v", rs)
	}
}
