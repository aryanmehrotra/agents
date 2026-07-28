// flaky-test-agent — a GoFr 1.58 service that mines CI run history for flaky tests and recommends
// quarantine. A flaky test is one that both passes and fails across runs of the same code — the most
// corrosive thing in a test suite, because it trains everyone to ignore red. Finding them is the test
// stage of the software-development lifecycle that keeps the other stages honest.
//
// The design deliberately inverts the usual "model proposes, Go disposes": here the DETECTION is the
// deterministic part and it lives entirely in Go. A test is flaky iff, in the runs you provide, it has
// at least one pass AND at least one fail — that's arithmetic over the data, not a judgement call, so
// the model never gets to decide what's flaky. The model's role is narrow and advisory: given the
// failure messages, it annotates each already-detected flaky test with a likely cause category and a
// one-line suggestion. If the model is unavailable the detection, ranking and quarantine list still
// stand — you just lose the annotations.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxRows            = 20000 // cap on input result rows
	maxMessagesPerTest = 5     // distinct failure messages kept per test
	maxMessageChars    = 300
)

// resultRow is one test's outcome in one CI run.
type resultRow struct {
	Test    string `json:"test"`
	Run     string `json:"run"`    // optional run id/label; ordering is otherwise the input order
	Status  string `json:"status"` // pass/fail (passed/failed/ok/error/… accepted)
	Message string `json:"message"`
}

// testStat is the deterministic per-test tally the flakiness verdict is computed from.
type testStat struct {
	Test        string   `json:"test"`
	Runs        int      `json:"runs"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	FailRate    float64  `json:"fail_rate"`
	Flips       int      `json:"flips"` // pass<->fail transitions in run order (higher = more erratic)
	Messages    []string `json:"messages,omitempty"`
	LikelyCause string   `json:"likely_cause,omitempty"` // model annotation (advisory)
	Suggestion  string   `json:"suggestion,omitempty"`   // model annotation (advisory)
	Quarantine  bool     `json:"quarantine"`
}

func main() {
	app := gofr.New()

	app.POST("/flaky", flaky)

	app.Run()
}

func flaky(c *gofr.Context) (any, error) {
	var in struct {
		Results []resultRow `json:"results"`
		Text    string      `json:"text"` // alias slot for the orchestrator's single-string call
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	if len(in.Results) == 0 {
		return map[string]any{
			"error": "provide `results`: an array of {test, status, run?, message?} rows across CI runs " +
				"(status pass/fail). Flakiness is detected from tests that both pass and fail across the runs.",
		}, nil
	}

	if len(in.Results) > maxRows {
		in.Results = in.Results[:maxRows]
	}

	flakyTests, alwaysFailing, healthy, totalRuns := analyze(in.Results)

	// Advisory only: annotate the already-detected flaky tests with a likely cause. Detection stands
	// without this — a model outage doesn't change which tests are flaky.
	annotate(c, flakyTests)

	return map[string]any{
		"summary": map[string]any{
			"tests":          len(flakyTests) + len(alwaysFailing) + healthy,
			"runs_observed":  totalRuns,
			"flaky":          len(flakyTests),
			"always_failing": len(alwaysFailing),
			"healthy":        healthy,
		},
		"flaky":          flakyTests,    // both passed AND failed across runs → quarantine candidates
		"always_failing": alwaysFailing, // failed every run → genuinely broken, NOT flaky (fix, don't quarantine)
		"note": "flakiness is detected deterministically in Go — a test is flaky iff it both passed and " +
			"failed across the runs provided. The model never decides what's flaky; it only annotates a " +
			"likely cause. Always-failing tests are reported separately: they are broken, not flaky.",
	}, nil
}

// analyze is the deterministic core: it tallies each test's passes/fails across the runs (in input
// order), classifies it as flaky (both outcomes seen), always-failing (only fails) or healthy (only
// passes), and ranks the flaky ones by fail rate then flip count. No model involved.
func analyze(rows []resultRow) (flakyTests, alwaysFailing []testStat, healthy, totalRuns int) {
	order := []string{}
	stats := map[string]*testStat{}
	last := map[string]string{}   // last status seen per test, for flip counting
	msgs := map[string][]string{} // distinct failure messages per test
	msgSeen := map[string]bool{}

	for _, r := range rows {
		name := strings.TrimSpace(r.Test)
		if name == "" {
			continue
		}

		status := normStatus(r.Status)
		if status == "" {
			continue // unknown status contributes nothing
		}

		totalRuns++

		s, ok := stats[name]
		if !ok {
			s = &testStat{Test: name}
			stats[name] = s
			order = append(order, name)
		}

		s.Runs++

		if status == "pass" {
			s.Passed++
		} else {
			s.Failed++

			if m := cleanMsg(r.Message); m != "" && !msgSeen[name+"\x00"+m] {
				msgSeen[name+"\x00"+m] = true
				if len(msgs[name]) < maxMessagesPerTest {
					msgs[name] = append(msgs[name], m)
				}
			}
		}

		if prev, seen := last[name]; seen && prev != status {
			s.Flips++
		}

		last[name] = status
	}

	flakyTests = []testStat{}
	alwaysFailing = []testStat{}

	for _, name := range order {
		s := stats[name]
		s.FailRate = round2(float64(s.Failed) / float64(s.Runs))
		s.Messages = msgs[name]

		switch {
		case s.Passed > 0 && s.Failed > 0: // flaky by definition
			s.Quarantine = true
			flakyTests = append(flakyTests, *s)
		case s.Failed > 0: // only ever failed → broken, not flaky
			alwaysFailing = append(alwaysFailing, *s)
		default:
			healthy++
		}
	}

	sort.SliceStable(flakyTests, func(i, j int) bool {
		if flakyTests[i].FailRate != flakyTests[j].FailRate {
			return flakyTests[i].FailRate > flakyTests[j].FailRate
		}

		return flakyTests[i].Flips > flakyTests[j].Flips
	})

	return flakyTests, alwaysFailing, healthy, totalRuns
}

// annotate asks the model for a likely-cause category and a one-line suggestion per flaky test, and
// merges them in. Best-effort: any error leaves the (already-valid) detection untouched.
func annotate(c *gofr.Context, flakyTests []testStat) {
	if len(flakyTests) == 0 {
		return
	}

	var b strings.Builder
	for _, s := range flakyTests {
		fmt.Fprintf(&b, "- %s (fail rate %.2f): %s\n", s.Test, s.FailRate, strings.Join(s.Messages, " | "))
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a CI reliability engineer. For each flaky test below " +
			"(name, fail rate, failure messages), give the single most likely cause as one of: " +
			"timing/race, test-ordering, external-dependency, resource/environment, nondeterministic-data, " +
			"unknown — plus a one-line fix suggestion. Reply with ONLY a JSON array of " +
			"{\"test\": string, \"likely_cause\": string, \"suggestion\": string}."},
		{Role: ai.RoleUser, Content: b.String()},
	}, ai.WithTemperature(0))
	if err != nil {
		c.Logger.Errorf("flaky: annotation model unavailable, returning detection only: %v", err)
		return
	}

	var ann []struct {
		Test       string `json:"test"`
		Cause      string `json:"likely_cause"`
		Suggestion string `json:"suggestion"`
	}

	if perr := json.Unmarshal([]byte(extractArray(resp.Content)), &ann); perr != nil {
		return
	}

	by := map[string]int{}
	for i := range flakyTests {
		by[flakyTests[i].Test] = i
	}

	for _, a := range ann {
		if i, ok := by[strings.TrimSpace(a.Test)]; ok {
			flakyTests[i].LikelyCause = strings.TrimSpace(a.Cause)
			flakyTests[i].Suggestion = strings.TrimSpace(a.Suggestion)
		}
	}
}

// normStatus folds the many ways CI reports pass/fail into "pass"/"fail" (or "" for unknown/skip).
func normStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pass", "passed", "ok", "success", "green", "true":
		return "pass"
	case "fail", "failed", "failure", "error", "red", "false":
		return "fail"
	default:
		return ""
	}
}

func cleanMsg(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxMessageChars {
		s = s[:maxMessageChars] + "…"
	}

	return s
}

func round2(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}

	return math.Round(f*100) / 100
}

// extractArray pulls the first balanced JSON array out of a model response that may wrap it in fences
// or prose; returns "[]" when there's none, so json.Unmarshal simply yields no annotations.
func extractArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return "[]"
	}

	depth := 0
	inStr := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]

		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inStr:
			escaped = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '[':
			depth++
		case ch == ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}

	return "[]"
}
