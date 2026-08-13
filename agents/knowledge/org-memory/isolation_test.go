package main

import (
	"context"
	"testing"
)

// TestTenantIsolationIsNotBypassable is P0-6. The red-team found sandbox decisions surfacing at rank
// #1 for an unrelated repo's query, and the diagnosis at the time was "scope is a soft prior by
// design, so scoping cannot contain an attacker". That was accurate about the mechanism and wrong as
// a resting place: tenancy is not a relevance question. A result from another tenant is not a
// less-relevant result, it is a leak, and that distinction must not be expressible as a knob.
func TestTenantIsolationIsNotBypassable(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	tenantA, err := en.Capture(ctx, Decision{
		What: "money must be stored as int64 cents never float", Scope: []string{"org:acme", "service:pay"},
	})
	if err != nil {
		t.Fatal(err)
	}

	shared, err := en.Capture(ctx, Decision{
		What: "money handling should avoid floating point rounding", Scope: []string{"service:pay"},
	})
	if err != nil {
		t.Fatal(err)
	}

	q := []string{"money stored as int64 cents"}

	t.Run("an unlabelled query cannot see labelled data", func(t *testing.T) {
		out, err := en.Recall(ctx, q)
		if err != nil {
			t.Fatal(err)
		}

		if containsID(out, tenantA.ID) {
			t.Error("a decision tagged org:acme must be invisible to a query that asserts no org")
		}

		if !containsID(out, shared.ID) {
			t.Error("untagged decisions must stay shared — a single-tenant deployment needs no config")
		}
	})

	t.Run("the wrong tenant cannot see it either", func(t *testing.T) {
		out, _ := en.Recall(ctx, append([]string{"org:evilcorp"}, q...))
		if containsID(out, tenantA.ID) {
			t.Error("org:evilcorp must not see org:acme data")
		}
	})

	t.Run("the right tenant can", func(t *testing.T) {
		out, _ := en.Recall(ctx, append([]string{"org:acme"}, q...))
		if !containsID(out, tenantA.ID) {
			t.Error("org:acme must see its own data")
		}
	})

	t.Run("no config can turn isolation off", func(t *testing.T) {
		// Every lever that weakens filtering elsewhere: soft scope (the DEFAULT), and an emptied
		// hard-facet list. Neither may reach the isolation check.
		en.cfg.Set("retrieve.hard_scope", "0")
		en.cfg.Set("retrieve.hard_facet_prefixes", "")

		out, _ := en.Recall(ctx, q)
		if containsID(out, tenantA.ID) {
			t.Error("isolation must not be disableable by configuration")
		}
	})
}

// TestHardFacetPrefixesAreConfigurable is P0-3: which facets are "a wrong value = a wrong answer" is
// a property of an org's tagging convention, not of this engine. `table:` is the motivating case —
// two true decisions about different tables were surfacing interchangeably, and the corpus had no way
// to say that a table name is a subject predicate rather than a topical hint.
func TestHardFacetPrefixesAreConfigurable(t *testing.T) {
	c := NewConfig()

	// Default: identity/type facets are hard, topical ones are not.
	got := hardFilterTags([]string{"author:vikash", "kind:book", "table:orders", "repo:x"}, c)
	if len(got) != 2 {
		t.Fatalf("default should treat only author:/kind: as hard, got %v", got)
	}

	// Opt `table:` in.
	c.Set("retrieve.hard_facet_prefixes", "author:,kind:,table:")

	got = hardFilterTags([]string{"author:vikash", "table:orders", "repo:x"}, c)
	if len(got) != 2 {
		t.Fatalf("table: should now be hard, got %v", got)
	}

	// A missing colon is syntax, not a decision — tolerate it.
	c.Set("retrieve.hard_facet_prefixes", "table")

	if got = hardFilterTags([]string{"table:orders"}, c); len(got) != 1 {
		t.Errorf(`"table" should be read as "table:", got %v`, got)
	}

	// Isolation prefixes are present no matter what the config says.
	c.Set("retrieve.hard_facet_prefixes", "")

	if got = hardFilterTags([]string{"org:acme", "author:vikash"}, c); len(got) != 1 || got[0] != "org:acme" {
		t.Errorf("org: must remain hard with an empty config list, got %v", got)
	}
}

// TestHardFacetSeparatesConfusableTables: the payoff for P0-3 — with `table:` opted in, a query that
// names its subject stops being answered by a decision about a different one.
func TestHardFacetSeparatesConfusableTables(t *testing.T) {
	en := newTestEngine()
	en.cfg.Set("retrieve.hard_facet_prefixes", "author:,kind:,table:")

	ctx := context.Background()

	cost, _ := en.Capture(ctx, Decision{
		What:  "clickhouse is the sole store with no mysql dual write",
		Scope: []string{"repo:acme", "table:orders"},
	})

	metrics, _ := en.Capture(ctx, Decision{
		What:  "mysql is the source of truth and clickhouse tables are replicas",
		Scope: []string{"repo:acme", "table:order_events"},
	})

	out, err := en.Recall(ctx, []string{"table:orders", "what is the source of truth for storage"})
	if err != nil {
		t.Fatal(err)
	}

	if containsID(out, metrics.ID) {
		t.Error("a query scoped to table:orders must not be answered by a order_events decision")
	}

	if len(out) > 0 && !containsID(out, cost.ID) {
		t.Errorf("the orders decision should be the one surfaced, got %+v", out)
	}
}

// TestScanBudgetDegradesWithoutBlocking is P0-5. §1.7 requires "budget-blown → falls back, never
// blocks". org-memory sits in an agent's prompt path, so a slow recall does not produce a worse
// answer — it stalls the agent. The scan therefore truncates at the deadline and SAYS SO, because an
// empty partial result and a genuine abstention are different claims.
func TestScanBudgetDegradesWithoutBlocking(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	for i := 0; i < 1200; i++ {
		if _, err := en.Capture(ctx, Decision{What: "retry backoff jitter variant " + itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}

	q := []string{"retry backoff jitter"}

	// No budget ⇒ full scan, and the result must be marked complete.
	_, diag, err := en.RecallWithDiag(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	if diag.BudgetExceeded {
		t.Error("no budget configured ⇒ the scan must never report itself truncated")
	}

	if diag.Scanned != diag.CorpusSize || diag.CorpusSize != 1200 {
		t.Fatalf("full scan expected, scanned %d of %d", diag.Scanned, diag.CorpusSize)
	}

	// An impossibly small budget ⇒ truncate, report it, and still return successfully.
	en.cfg.Set("retrieve.budget_ms", "0.0001")

	items, diag, err := en.RecallWithDiag(ctx, q)
	if err != nil {
		t.Fatalf("a blown budget must degrade, not error: %v", err)
	}

	if !diag.BudgetExceeded {
		t.Fatal("an exhausted budget must be reported, not hidden")
	}

	if diag.Scanned >= diag.CorpusSize {
		t.Errorf("a truncated scan should cover less than the corpus, got %d of %d", diag.Scanned, diag.CorpusSize)
	}

	if items == nil && diag.Scanned == 0 {
		t.Error("degradation must still return whatever was found, never nothing-by-default")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}

	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}

	return string(b)
}

// TestListEnforcesIsolation closes the strongest counterexample to this system's "isolation is not
// bypassable" claim. /recall correctly refused an `org:acme` decision to an unscoped caller, while
// /decisions — an unauthenticated GET serving the dashboard's whole-brain view — handed the same
// decision to anybody. Enforcing a boundary on the query path and not on the bulk-read path enforces
// nothing: an attacker uses the endpoint that does not check.
func TestListEnforcesIsolation(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	secret, _ := en.Capture(ctx, Decision{What: "ACME rotate the billing key", Scope: []string{"org:acme"}})
	shared, _ := en.Capture(ctx, Decision{What: "prefer table driven tests"})

	has := func(list []DecisionView, id string) bool {
		for _, v := range list {
			if v.Decision.ID == id {
				return true
			}
		}

		return false
	}

	if got := en.List(); has(got, secret.ID) {
		t.Error("an unscoped bulk read must not expose org:acme data")
	} else if !has(got, shared.ID) {
		t.Error("unlabelled decisions must remain visible — a single-tenant deployment needs no config")
	}

	if got := en.List("org:evilcorp"); has(got, secret.ID) {
		t.Error("the wrong tenant must not see it either")
	}

	if got := en.List("org:acme"); !has(got, secret.ID) {
		t.Error("the owning tenant must see its own data")
	}
}
