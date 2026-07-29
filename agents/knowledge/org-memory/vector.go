package main

import "math"

// cosine returns cosine similarity in [-1,1]. Returns 0 when either vector is empty, zero, or the
// lengths differ — so a missing embedding never falsely looks "relevant". Phase-0 recall does exact
// brute-force cosine over candidates: at this scale it is sub-millisecond AND exact (no ANN recall
// loss), which is the most correct option; an index is only worth it at millions of vectors.
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}

	var dot, na, nb float64

	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}

	if na == 0 || nb == 0 {
		return 0
	}

	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
