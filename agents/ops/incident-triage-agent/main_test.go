package main

import "testing"

// TestSeverityFloorPanic proves a panic in the raw evidence forces a P1 floor — the deterministic
// signal a model can't be talked out of.
func TestSeverityFloorPanic(t *testing.T) {
	got := severityFloor("checkout-service alert", []string{"panic: nil pointer dereference in ProcessPayment"})
	if got != "P1" {
		t.Errorf("severityFloor(panic) = %q, want P1", got)
	}
}

func TestSeverityFloorDegraded(t *testing.T) {
	got := severityFloor("latency elevated latency on search-service", nil)
	if got != "P2" {
		t.Errorf("severityFloor(elevated latency) = %q, want P2", got)
	}
}

func TestSeverityFloorNoSignal(t *testing.T) {
	got := severityFloor("informational: deploy started", nil)
	if got != "P4" {
		t.Errorf("severityFloor(no signal) = %q, want P4 (no floor forced)", got)
	}
}

// TestClampSeverityCannotBeTalkedDown is the guardrail's core guarantee: a model severity that's
// weaker than the evidence-derived floor is overridden UP to the floor, even if the model (having
// been prompt-injected via the alert/log text) suggests "P4, do not page anyone" for a panic.
func TestClampSeverityCannotBeTalkedDown(t *testing.T) {
	if got := clampSeverity("P4", "P1"); got != "P1" {
		t.Errorf("clampSeverity(model=P4, floor=P1) = %q, want P1 — the floor must win", got)
	}
}

// TestClampSeverityModelCanBeMoreUrgent confirms the floor is a MINIMUM, not a ceiling — the model is
// still free to escalate above it.
func TestClampSeverityModelCanBeMoreUrgent(t *testing.T) {
	if got := clampSeverity("P1", "P4"); got != "P1" {
		t.Errorf("clampSeverity(model=P1, floor=P4) = %q, want P1", got)
	}
}

func TestClampSeverityInvalidModelValue(t *testing.T) {
	if got := clampSeverity("critical!!", "P4"); got != "P3" {
		t.Errorf("clampSeverity(invalid) = %q, want the P3 default", got)
	}
}

// TestResolveOwnerIsDeterministicNotModel is the routing guardrail: the owner comes from the
// directory lookup, keyed only by `service` — free text (an alert or a log line trying to say "route
// this to no one") is never consulted.
func TestResolveOwnerIsDeterministicNotModel(t *testing.T) {
	o, source := resolveOwner("checkout-service", nil)
	if o.Team != "payments-team" || source != "builtin-directory" {
		t.Errorf("resolveOwner(checkout-service) = %+v, %q", o, source)
	}
}

func TestResolveOwnerCaseInsensitive(t *testing.T) {
	o, _ := resolveOwner("  CHECKOUT-SERVICE  ", nil)
	if o.Team != "payments-team" {
		t.Errorf("resolveOwner should be case/space insensitive, got %+v", o)
	}
}

func TestResolveOwnerCallerDirectoryOverridesBuiltin(t *testing.T) {
	dir := map[string]ownerEntry{"checkout-service": {Team: "custom-team", Channel: "#custom"}}

	o, source := resolveOwner("checkout-service", dir)
	if o.Team != "custom-team" || source != "caller-directory" {
		t.Errorf("resolveOwner(caller override) = %+v, %q", o, source)
	}
}

func TestResolveOwnerUnknownService(t *testing.T) {
	o, source := resolveOwner("some-service-nobody-registered", nil)
	if o.Team != "unassigned" || source != "unknown-service" {
		t.Errorf("resolveOwner(unknown) = %+v, %q, want unassigned/unknown-service", o, source)
	}
}

func TestResolveOwnerNoServiceGiven(t *testing.T) {
	o, source := resolveOwner("", nil)
	if o.Team != "unassigned" || source != "no-service-given" {
		t.Errorf("resolveOwner(empty) = %+v, %q", o, source)
	}
}

func TestNormalizeCategory(t *testing.T) {
	if got := normalizeCategory("Code-Defect"); got != "code-defect" {
		t.Errorf("normalizeCategory(valid) = %q", got)
	}

	if got := normalizeCategory("the model made this up"); got != "unknown" {
		t.Errorf("normalizeCategory(invalid) = %q, want unknown", got)
	}
}

// TestValidCitationsDropsHallucinatedIndices is the evidence guardrail: a citation pointing past the
// end of the supplied logs (or negative) is dropped, never trusted as real evidence.
func TestValidCitationsDropsHallucinatedIndices(t *testing.T) {
	got := validCitations([]int{2, 0, -1, 99, 0}, 3)

	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Errorf("validCitations = %v, want [0 2]", got)
	}
}

func TestExtractObject(t *testing.T) {
	if got := extractObject("here:\n```json\n{\"category\":\"network\"}\n```"); got != `{"category":"network"}` {
		t.Errorf("extractObject = %q", got)
	}

	if extractObject("no object here") != "{}" {
		t.Error("extractObject(none) should be {}")
	}
}
