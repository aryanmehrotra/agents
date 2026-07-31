package main

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

// §1.7 requires "recall p95 < budget" with a dense-only fallback when the budget blows. Neither the
// measurement nor the fallback existed, so "p95 < budget" was an aspiration with no number on either
// side of the "<". These benchmarks supply the left-hand side, and specifically isolate the part
// that GROWS: the exact brute-force cosine scan is O(N) in corpus size, so it — not the embedding
// call — is what eventually breaks the budget. Run:
//
//	go test -bench=BenchmarkRecallScan -benchtime=200x -run=XXX
//
// Uses the deterministic fake embedder, so this measures OUR scan and ranking, not Ollama's latency
// or the network. Measured separately: the live end-to-end p95 is ~76 ms, of which the Ollama embed
// round-trip is the overwhelming majority — see PHASE0-AUDIT.md.
func BenchmarkRecallScan(b *testing.B) {
	for _, n := range []int{1000, 10000, 25000} {
		b.Run(fmt.Sprintf("corpus=%d", n), func(b *testing.B) {
			en := benchEngine(b, n)
			ctx := context.Background()
			q := []string{"retry backoff exponential jitter for downstream calls"}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := en.Recall(ctx, q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchEngine builds an in-memory engine holding n synthetic decisions with varied text, so the
// cosine scan does real work rather than hitting one degenerate vector.
func benchEngine(b *testing.B, n int) *Engine {
	b.Helper()

	// Production embedding width, NOT newTestEngine's 4096-wide fake: the cosine scan is linear in
	// dimension, so benchmarking at 4096 overstates the real cost by 5.3× and would have produced a
	// scaling story that is wrong in the direction that matters (too pessimistic → premature ANN work).
	cfg := NewConfig()
	cfg.Set("retrieve.precision_floor", "0.15")

	en := NewEngine(newMemStore(), fakeEmbedder{dim: nomicEmbedDim}, cfg)
	ctx := context.Background()

	//nolint:gosec // deterministic seed: benchmark corpora must be reproducible across runs
	rng := rand.New(rand.NewSource(1))

	topics := []string{
		"retry backoff exponential jitter", "circuit breaker half open probe",
		"graceful shutdown drain connections", "rate limit token bucket",
		"cache invalidation ttl stampede", "database migration rollback",
		"structured logging correlation id", "tracing span context propagation",
	}

	for i := 0; i < n; i++ {
		t := topics[rng.Intn(len(topics))]

		if _, err := en.Capture(ctx, Decision{
			What:  fmt.Sprintf("%s variant %d", t, i),
			Why:   fmt.Sprintf("rationale %d for %s", i, t),
			Scope: []string{fmt.Sprintf("repo:r%d", i%16), fmt.Sprintf("service:s%d", i%7)},
		}); err != nil {
			b.Fatal(err)
		}
	}

	return en
}

// nomicEmbedDim is the width of nomic-embed-text, the model this deployment actually runs. Named
// here so the benchmark cannot silently drift from production geometry.
const nomicEmbedDim = 768
