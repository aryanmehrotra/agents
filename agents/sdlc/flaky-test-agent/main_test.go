package main

import "testing"

// TestAnalyzeClassifies is the core guarantee: a test with both passes and fails is flaky; one that
// only ever fails is always-failing (not flaky); one that only passes is healthy.
func TestAnalyzeClassifies(t *testing.T) {
	rows := []resultRow{
		{Test: "TestFlaky", Status: "pass"},
		{Test: "TestFlaky", Status: "fail", Message: "timeout waiting for server"},
		{Test: "TestFlaky", Status: "pass"},
		{Test: "TestFlaky", Status: "fail", Message: "timeout waiting for server"}, // dup msg → deduped
		{Test: "TestBroken", Status: "fail", Message: "nil pointer"},
		{Test: "TestBroken", Status: "fail", Message: "nil pointer"},
		{Test: "TestSolid", Status: "pass"},
		{Test: "TestSolid", Status: "pass"},
	}

	flaky, always, healthy, runs := analyze(rows)

	if len(flaky) != 1 || flaky[0].Test != "TestFlaky" {
		t.Fatalf("flaky = %#v, want just TestFlaky", flaky)
	}

	if len(always) != 1 || always[0].Test != "TestBroken" {
		t.Fatalf("always_failing = %#v, want just TestBroken", always)
	}

	if healthy != 1 {
		t.Errorf("healthy = %d, want 1", healthy)
	}

	if runs != 8 {
		t.Errorf("runs = %d, want 8", runs)
	}

	f := flaky[0]
	if f.Passed != 2 || f.Failed != 2 || f.FailRate != 0.5 {
		t.Errorf("TestFlaky tally = passed %d failed %d rate %v", f.Passed, f.Failed, f.FailRate)
	}

	if !f.Quarantine {
		t.Error("a flaky test should be recommended for quarantine")
	}

	if len(f.Messages) != 1 { // the duplicate failure message is deduped
		t.Errorf("messages = %#v, want 1 deduped", f.Messages)
	}

	if always[0].Quarantine {
		t.Error("an always-failing test is broken, not flaky — do NOT quarantine")
	}
}

// TestAnalyzeFlips counts pass<->fail transitions in run order — a proxy for how erratic a test is.
func TestAnalyzeFlips(t *testing.T) {
	rows := []resultRow{
		{Test: "T", Status: "pass"},
		{Test: "T", Status: "fail"},
		{Test: "T", Status: "pass"},
		{Test: "T", Status: "fail"}, // pass,fail,pass,fail → 3 flips
	}

	flaky, _, _, _ := analyze(rows)
	if flaky[0].Flips != 3 {
		t.Errorf("flips = %d, want 3", flaky[0].Flips)
	}
}

// TestAnalyzeRanking orders flaky tests by fail rate (highest first).
func TestAnalyzeRanking(t *testing.T) {
	rows := []resultRow{
		{Test: "Low", Status: "pass"}, {Test: "Low", Status: "pass"},
		{Test: "Low", Status: "pass"}, {Test: "Low", Status: "fail"}, // 0.25
		{Test: "High", Status: "fail"}, {Test: "High", Status: "fail"},
		{Test: "High", Status: "fail"}, {Test: "High", Status: "pass"}, // 0.75
	}

	flaky, _, _, _ := analyze(rows)
	if len(flaky) != 2 || flaky[0].Test != "High" {
		t.Errorf("ranking = %v, want High first (higher fail rate)", []string{flaky[0].Test, flaky[1].Test})
	}
}

// TestNormStatus folds the many CI spellings into pass/fail, and ignores unknown.
func TestNormStatus(t *testing.T) {
	for _, s := range []string{"PASS", "passed", "ok", "success", "green"} {
		if normStatus(s) != "pass" {
			t.Errorf("normStatus(%q) != pass", s)
		}
	}

	for _, s := range []string{"FAIL", "failed", "error", "red"} {
		if normStatus(s) != "fail" {
			t.Errorf("normStatus(%q) != fail", s)
		}
	}

	if normStatus("skipped") != "" {
		t.Error("unknown status should be ignored")
	}
}

// TestExtractArray recovers a fenced JSON array and yields "[]" when there's none.
func TestExtractArray(t *testing.T) {
	if got := extractArray("here:\n```json\n[{\"test\":\"T\"}]\n```"); got != `[{"test":"T"}]` {
		t.Errorf("extractArray = %q", got)
	}

	if extractArray("no array") != "[]" {
		t.Error("extractArray(none) should be []")
	}
}
