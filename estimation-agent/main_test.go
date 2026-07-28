package main

import (
	"math"
	"testing"
)

// TestNormalize maps sizes to points deterministically, defaults an unrecognized confidence to
// medium, and moves a task with an unusable size to `invalid` rather than counting it as zero.
func TestNormalize(t *testing.T) {
	sized, invalid := normalize([]map[string]any{
		{"title": "wire the route", "size": "M", "confidence": "high", "rationale": "small handler"},
		{"title": "migrate schema", "size": "XL", "confidence": "wat"}, // bad confidence → medium
		{"title": "mystery", "size": "huge"},                           // bad size → invalid
		{"title": "", "size": "S"},                                     // no title → skipped
	})

	if len(sized) != 2 || len(invalid) != 1 {
		t.Fatalf("normalize split = %d sized, %d invalid", len(sized), len(invalid))
	}

	if sized[0].Points != 3 { // M = 3
		t.Errorf("M mapped to %d points, want 3", sized[0].Points)
	}

	// high confidence → ±10% band on 3 points.
	if sized[0].LowPoints != 2.7 || sized[0].HighPoints != 3.3 {
		t.Errorf("high-confidence band = [%v,%v], want [2.7,3.3]", sized[0].LowPoints, sized[0].HighPoints)
	}

	if sized[1].Confidence != "medium" {
		t.Errorf("unrecognized confidence = %q, want defaulted to medium", sized[1].Confidence)
	}

	if invalid[0].Title != "mystery" {
		t.Errorf("invalid[0] = %q, want the bad-size task", invalid[0].Title)
	}
}

// TestAggregate is the guardrail's core: the total is Go's sum of per-task points, and the range is
// the sum of per-task confidence bands — never a number the model supplied.
func TestAggregate(t *testing.T) {
	sized := []sizedTask{
		{Points: 3, LowPoints: 2.7, HighPoints: 3.3},  // M, high
		{Points: 8, LowPoints: 4.0, HighPoints: 12.0}, // XL, low
	}

	est := aggregate(sized, 0, defSprintDays)
	pts := est["points"].(map[string]any)

	if pts["likely"] != 11.0 {
		t.Errorf("likely = %v, want 11 (3+8)", pts["likely"])
	}

	if pts["optimistic"] != 6.7 || pts["pessimistic"] != 15.3 {
		t.Errorf("range = [%v,%v], want [6.7,15.3]", pts["optimistic"], pts["pessimistic"])
	}

	if _, hasDur := est["duration"]; hasDur {
		t.Error("no velocity given → duration should be absent")
	}
}

// TestAggregateDuration divides the point range through the team's velocity to get working days.
func TestAggregateDuration(t *testing.T) {
	sized := []sizedTask{{Points: 10, LowPoints: 9, HighPoints: 11}}

	est := aggregate(sized, 5, 10) // 5 points/sprint, 10 days/sprint → 2 days/point
	dur, ok := est["duration"].(map[string]any)

	if !ok {
		t.Fatal("velocity given → duration should be present")
	}

	if dur["likely_days"] != 20.0 { // 10/5*10
		t.Errorf("likely_days = %v, want 20", dur["likely_days"])
	}
}

// TestSniffClaimedTotal extracts the model's volunteered total (the arithmetic Go ignores), and
// returns nil when the model didn't offer one.
func TestSniffClaimedTotal(t *testing.T) {
	if got := sniffClaimedTotal(`blah "total_points": 42, more`); got == nil || *got != 42 {
		t.Errorf("sniffClaimedTotal = %v, want 42", got)
	}

	if got := sniffClaimedTotal(`[{"title":"x","size":"M"}]`); got != nil {
		t.Errorf("sniffClaimedTotal(no total) = %v, want nil", got)
	}
}

// TestExtractTaskArray recovers the array from a fenced response and surfaces a claimed total; it
// errors when there's no array.
func TestExtractTaskArray(t *testing.T) {
	tasks, total, err := extractTaskArray("here:\n```json\n[{\"title\":\"a\",\"size\":\"S\"}]\n```\ntotal: 2")
	if err != nil || len(tasks) != 1 || tasks[0]["size"] != "S" {
		t.Fatalf("extractTaskArray = %#v, %v, %v", tasks, total, err)
	}

	if _, _, err := extractTaskArray("no array at all"); err == nil {
		t.Error("extractTaskArray(no array) should error")
	}
}

// TestTaskTitles joins detail onto the title and caps the count.
func TestTaskTitles(t *testing.T) {
	got := taskTitles([]map[string]any{
		{"title": "build", "detail": "the handler"},
		{"detail": "no title"},
		{"title": "ship"},
	})

	if len(got) != 2 || got[0] != "build — the handler" || got[1] != "ship" {
		t.Errorf("taskTitles = %#v", got)
	}
}

func TestRound1(t *testing.T) {
	if round1(2.7500001) != 2.8 || round1(3.0) != 3.0 {
		t.Errorf("round1 wrong: %v %v", round1(2.7500001), round1(3.0))
	}

	if math.Abs(round1(4.05)-4.1) > 1e-9 && math.Abs(round1(4.05)-4.0) > 1e-9 {
		t.Errorf("round1(4.05) = %v", round1(4.05))
	}
}
