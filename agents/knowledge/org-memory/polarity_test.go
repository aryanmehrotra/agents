package main

import "testing"

// TestPolarityGuardBlocksInvertedAdvice: the single worst output this system can produce is the
// exact reverse of what was asked, delivered with maximum confidence. Measured repeatedly at
// weak=false, noise_p ~1e-13 — no similarity or confidence signal can see it, because the two texts
// are topically identical and differ by one token.
func TestPolarityGuardBlocksInvertedAdvice(t *testing.T) {
	cfg := NewConfig()

	for _, tc := range []struct {
		name, query, doc string
		want             bool
	}{
		{"antonym open/closed", "should we fail closed when the store is down",
			"Fail open (allow the request) when the backing store is unavailable", true},
		{"negation scope", "do not retry failed requests",
			"Retry failed calls with exponential backoff plus jitter", true},
		{"antonym enable/disable", "disable rate limiting",
			"Implement rate limiting server-side as middleware", true},

		// Must NOT fire — these are legitimate answers and blocking them would be worse than the bug.
		{"same polarity", "should we fail closed on auth errors",
			"Fail closed on auth service errors, deny the request", false},
		{"doc covers both sides", "should we fail closed",
			"Fail open on cache errors but fail closed on auth errors", false},
		{"unrelated topic", "how do we log requests",
			"Emit structured JSON logs with a correlation id", false},
		{"negation in BOTH", "do not retry failed requests",
			"Do not retry non-idempotent writes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := polarityConflict(tc.query, tc.doc, cfg); got != tc.want {
				t.Errorf("polarityConflict = %v, want %v\n  q: %s\n  d: %s", got, tc.want, tc.query, tc.doc)
			}
		})
	}
}

// TestEntityGuardSeparatesSubjects: two decisions can both be TRUE and still make each other wrong
// answers, because they are about different subjects. Dense retrieval conflated them, BM25 could not
// bridge "orders" to "order_events", and a cross-encoder failed too — but the corpus already carries
// the answer as a facet, and structure beats inference.
func TestEntityGuardSeparatesSubjects(t *testing.T) {
	prefixes := []string{"table:", "service:"}
	asked := set([]string{"source", "truth", "orders"})

	if !entityConflict(asked, []string{"repo:acme", "table:order_events"}, prefixes) {
		t.Error("a decision about table:order_events must not answer a question naming orders")
	}

	if entityConflict(asked, []string{"repo:acme", "table:orders"}, prefixes) {
		t.Error("a decision about the queried table MUST still answer")
	}

	// Unfaceted decisions are never blocked — most of a corpus carries no entity facet at all.
	if entityConflict(asked, []string{"repo:acme"}, prefixes) {
		t.Error("an unfaceted decision must not be dropped")
	}

	// Facets outside the configured entity families are topical, not subjects.
	if entityConflict(asked, []string{"repo:acme", "kind:book"}, prefixes) {
		t.Error("a non-entity facet must not trigger the guard")
	}

	// The precondition: with a query naming no known entity, the guard must stay off entirely —
	// otherwise every scoped decision disappears from every ordinary question.
	corpus := map[string]bool{"orders": true, "order_events": true}
	if queryNamesEntity(set([]string{"how", "should", "log", "requests"}), corpus) {
		t.Error("a query naming no entity must not arm the guard")
	}

	if !queryNamesEntity(asked, corpus) {
		t.Error("a query naming a known entity must arm the guard")
	}
}
