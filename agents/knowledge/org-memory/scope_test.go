package main

import "testing"

func TestIDForDedupAndScopeInsensitivity(t *testing.T) {
	a := idFor("Use int64 for money", []string{"repo:x", "service:pay"})
	b := idFor("  use INT64 for money ", []string{"service:pay", "repo:x"}) // diff case/order/spacing
	if a != b {
		t.Fatalf("same knowledge must hash equal (dedupe): %s vs %s", a, b)
	}

	if c := idFor("Use float for money", []string{"repo:x"}); a == c {
		t.Fatal("different knowledge must not collide")
	}

	if len(a) < 3 || a[:2] != "d-" {
		t.Fatalf("bad id format: %s", a)
	}
}

func TestScopeMatch(t *testing.T) {
	ctx := contextSet([]string{"repo:x", "service:pay", "lang:go"})

	if ok, spec := scopeMatch(nil, ctx); !ok || spec != 0 {
		t.Fatalf("global (no tags) should match any with spec 0, got ok=%v spec=%d", ok, spec)
	}

	if ok, spec := scopeMatch([]string{"repo:x", "service:pay"}, ctx); !ok || spec != 2 {
		t.Fatalf("subset should match with spec 2, got ok=%v spec=%d", ok, spec)
	}

	if ok, _ := scopeMatch([]string{"repo:y"}, ctx); ok {
		t.Fatal("out-of-context scope must not match")
	}

	if ok, spec := scopeMatch([]string{"global"}, ctx); !ok || spec != 0 {
		t.Fatalf("literal 'global' should match any with spec 0, got ok=%v spec=%d", ok, spec)
	}
}
