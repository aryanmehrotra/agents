package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The retrieval gate, runnable by ANYONE who clones this repo.
//
// The committed precision@k harness (eval/validation/precision_at_k.py) talks to a live service
// holding a private 1,646-decision corpus. That made it unrunnable for everybody else — a quality
// gate nobody but its author can execute is not a gate, it is a claim. Worse, it silently rots: the
// queries were sanitised for publication while the private corpus kept the original wording, and
// five of eighteen "failures" turned out to be that drift rather than any real regression.
//
// So the gate also lives here, over a small fixture corpus committed alongside it. Deterministic
// embedder, no service, no network, no private data: `go test` runs it anywhere. The fixture is
// deliberately small and neutral — it exists to pin BEHAVIOURS (does an opposite-polarity question
// get the reversed rule?), not to simulate a real corpus.

type goldenCase struct {
	Q              string `json:"q"`
	MustContain    string `json:"must_contain"`
	MustNotContain string `json:"must_not_contain"`
	Note           string `json:"note"`
}

func loadJSONL[T any](t *testing.T, path string) []T {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []T

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		out = append(out, v)
	}

	return out
}

// fixtureEngine loads the committed fixture corpus into a fresh in-memory engine.
func fixtureEngine(t *testing.T) (*Engine, context.Context) {
	t.Helper()

	cfg := NewConfig()
	cfg.Set("retrieve.precision_floor", "0.12")
	cfg.Set("retrieve.top_k", "3")

	en := NewEngine(newMemStore(), newFakeEmbedder(), cfg)
	ctx := context.Background()

	for _, d := range loadJSONL[Decision](t, filepath.Join("eval", "validation", "fixture_corpus.jsonl")) {
		if _, err := en.Capture(ctx, d); err != nil {
			t.Fatalf("seed %q: %v", d.What, err)
		}
	}

	return en, ctx
}

// TestGoldenRetrieval is the portable precision gate.
//
// Two criteria, because retrieval fails in two different ways and one of them is invisible to the
// other. `must_contain` catches "we missed the answer". `must_not_contain` catches "we returned the
// OPPOSITE" — which a must_contain check scores as an ordinary miss, indistinguishable from the
// harmless case of finding nothing. For a negation query, ABSTAINING IS A PASS: "I have no rule
// about failing closed" is honest, "here is the rule about failing open" is not.
func TestGoldenRetrieval(t *testing.T) {
	en, ctx := fixtureEngine(t)

	cases := loadJSONL[goldenCase](t, filepath.Join("eval", "validation", "golden_fixture_queries.jsonl"))
	if len(cases) == 0 {
		t.Fatal("no golden cases loaded")
	}

	var failed int

	for _, c := range cases {
		out, err := en.Recall(ctx, []string{c.Q})
		if err != nil {
			t.Fatalf("recall %q: %v", c.Q, err)
		}

		var blob strings.Builder
		for _, it := range out {
			blob.WriteString(strings.ToLower(it.Decision.What + " " + it.Decision.Why + " "))
		}

		got := blob.String()

		if c.MustContain != "" && !strings.Contains(got, strings.ToLower(c.MustContain)) {
			failed++

			t.Errorf("MISS %q\n     wanted %q\n     got   %.90q", c.Q, c.MustContain, got)
		}

		if c.MustNotContain != "" && strings.Contains(got, strings.ToLower(c.MustNotContain)) {
			failed++

			t.Errorf("OPPOSITE %q\n     must not contain %q — returning the reverse of what was asked\n     got  %.90q",
				c.Q, c.MustNotContain, got)
		}
	}

	t.Logf("golden: %d/%d passed", len(cases)-failed, len(cases))
}
