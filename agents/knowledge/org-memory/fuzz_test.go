package main

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// Fuzz the pure text functions. None of these has ever seen a fuzzer.
func FuzzSanitizeAdvice(f *testing.F) {
	for _, s := range []string{"", "a\nb", "\x00\x07", "safe‮txet", strings.Repeat("x", 5000), "🙂🙂"} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got := sanitizeAdvice(s, 100)

		if strings.ContainsAny(got, "\n\r\t\x00") {
			t.Fatalf("control char survived: %q -> %q", s, got)
		}

		if !utf8.ValidString(got) && utf8.ValidString(s) {
			t.Fatalf("valid input became invalid utf8: %q -> %q", s, got)
		}

		if n := utf8.RuneCountInString(got); n > 101 {
			t.Fatalf("length cap breached: %d runes", n)
		}
	})
}

func FuzzNormalizeAndSubstance(f *testing.F) {
	for _, s := range []string{"", "  ", "!!!", "Should we fail CLOSED?", "orders", "日本語"} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		n := normalizeGapQuery(s)

		if strings.HasPrefix(n, " ") || strings.HasSuffix(n, " ") || strings.Contains(n, "  ") {
			t.Fatalf("normalisation left ragged whitespace: %q -> %q", s, n)
		}

		if normalizeGapQuery(n) != n {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, n, normalizeGapQuery(n))
		}

		if substanceTokens(s) > len(lexTokens(s)) {
			t.Fatalf("substance cannot exceed total tokens: %q", s)
		}
	})
}

// The bounded-prior and decay claims, fuzzed rather than argued.
func FuzzHalfLifeDecayBounded(f *testing.F) {
	f.Add(0.0, 90.0)
	f.Add(1e9, 1.0)
	f.Add(-5.0, 90.0)

	f.Fuzz(func(t *testing.T, days, half float64) {
		if math.IsNaN(days) || math.IsNaN(half) || math.IsInf(days, 0) || math.IsInf(half, 0) {
			t.Skip()
		}

		got := halfLifeDecay(days, half)
		if math.IsNaN(got) || got < 0 || got > 1 {
			t.Fatalf("decay escaped [0,1]: days=%v half=%v -> %v", days, half, got)
		}
	})
}

func FuzzNoiseModelNeverLies(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5})

	f.Fuzz(func(t *testing.T, b []byte) {
		sims := make([]float64, 0, len(b))
		for _, v := range b {
			sims = append(sims, float64(v)/255)
		}

		m := fitNoiseModel(sims, 5)

		p := m.tailProb(0.5)
		if math.IsNaN(p) || p < 0 || p > 1 {
			t.Fatalf("tailProb is not a probability: %v (n=%d)", p, len(sims))
		}

		if !m.ok && p != 1 {
			t.Fatalf("an unfitted model must claim nothing (p=1), got %v", p)
		}

		if s, ok := m.scoreAtTailProb(0.01); ok && (math.IsNaN(s) || math.IsInf(s, 0)) {
			t.Fatalf("threshold is not finite: %v", s)
		}
	})
}
