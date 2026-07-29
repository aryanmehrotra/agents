package main

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	a := []float32{1, 0, 0}

	if got := cosine(a, a); math.Abs(got-1) > 1e-9 {
		t.Fatalf("identical vectors should be 1, got %v", got)
	}

	if got := cosine(a, []float32{0, 1, 0}); math.Abs(got) > 1e-9 {
		t.Fatalf("orthogonal should be 0, got %v", got)
	}

	if got := cosine(a, []float32{1, 0}); got != 0 {
		t.Fatalf("mismatched length should be 0, got %v", got)
	}

	if got := cosine(nil, a); got != 0 {
		t.Fatalf("empty should be 0, got %v", got)
	}

	if got := cosine(a, []float32{0, 0, 0}); got != 0 {
		t.Fatalf("zero vector should be 0, got %v", got)
	}
}
