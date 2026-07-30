package main

import (
	"math"
	"math/rand"
	"testing"
)

// noiseSample builds a realistic query score distribution: a large irrelevant bulk plus `relevant`
// documents in the right tail. Deterministic so the assertions below are exact.
func noiseSample(bulk, relevant int) []float64 {
	//nolint:gosec // deterministic seed: a statistical test must be reproducible
	rng := rand.New(rand.NewSource(7))

	out := make([]float64, 0, bulk+relevant)
	for i := 0; i < bulk; i++ {
		out = append(out, rng.NormFloat64()*0.06+0.45)
	}

	for i := 0; i < relevant; i++ {
		out = append(out, rng.NormFloat64()*0.03+0.80)
	}

	return out
}

// TestNoiseModelResistsRelevantContamination is the reason this whole file exists.
//
// The previous adaptive floor was mean + z·σ over ALL candidate similarities — including the relevant
// ones it was supposed to admit. Relevant documents live in the right tail, so they inflated both
// moments and pushed the floor UP: the gate got HARDER exactly when a query had good answers, and a
// query with many relevant decisions was penalised relative to one with few. The robust estimator
// (median / MAD, 50% breakdown point) measures the noise even though signal is mixed into the sample.
func TestNoiseModelResistsRelevantContamination(t *testing.T) {
	base := fitNoiseModel(noiseSample(1600, 0), 30)
	if !base.ok {
		t.Fatal("a 1600-point sample must fit")
	}

	for _, relevant := range []int{3, 20, 60, 150} {
		m := fitNoiseModel(noiseSample(1600, relevant), 30)

		if !m.ok {
			t.Fatalf("relevant=%d: model should still fit", relevant)
		}

		// The estimated noise scale must barely move as relevant documents are added.
		if drift := math.Abs(m.scale-base.scale) / base.scale; drift > 0.10 {
			t.Errorf("relevant=%d: noise scale drifted %.1f%% (robust estimator should be stable)",
				relevant, drift*100)
		}

		// And so must the derived threshold — that is the property the floor depends on.
		got, _ := m.scoreAtTailProb(0.01)
		want, _ := base.scoreAtTailProb(0.01)

		if math.Abs(got-want) > 0.02 {
			t.Errorf("relevant=%d: threshold moved %.4f → %.4f just because answers exist",
				relevant, want, got)
		}
	}
}

// TestNoiseModelTailProbIsCalibrated: the number must actually behave like a probability, otherwise
// it is just another opaque score wearing a percentage sign.
func TestNoiseModelTailProbIsCalibrated(t *testing.T) {
	m := fitNoiseModel(noiseSample(4000, 0), 30)
	if !m.ok {
		t.Fatal("fit failed")
	}

	// At the centre of the noise, half of it lies above.
	if p := m.tailProb(m.center); math.Abs(p-0.5) > 0.01 {
		t.Errorf("tailProb at the median should be ~0.5, got %.4f", p)
	}

	// Monotone decreasing in score — a better match can never look MORE like noise.
	prev := 1.0

	for s := 0.30; s <= 0.95; s += 0.05 {
		p := m.tailProb(s)
		if p > prev+1e-12 {
			t.Fatalf("tailProb must be non-increasing; at s=%.2f got %.6f after %.6f", s, p, prev)
		}

		prev = p
	}

	// Round-trip: the score at a given tail probability reports back that probability.
	for _, alpha := range []float64{0.5, 0.1, 0.01, 0.001} {
		s, ok := m.scoreAtTailProb(alpha)
		if !ok {
			t.Fatalf("alpha=%g: inversion failed", alpha)
		}

		if p := m.tailProb(s); math.Abs(p-alpha)/alpha > 0.01 {
			t.Errorf("alpha=%g: round-trip gave %g", alpha, p)
		}
	}
}

// TestNoiseModelDeclinesWhenItCannotKnow: an unfittable sample must report "no claim", never a
// confident-looking default. Silent fabrication of confidence is the failure this system exists to
// avoid — the same defect as the masked `top_similarity: 0.0`.
func TestNoiseModelDeclinesWhenItCannotKnow(t *testing.T) {
	if m := fitNoiseModel(noiseSample(10, 0), 30); m.ok {
		t.Error("a 10-point sample must not produce a model when minN is 30")
	}

	// Degenerate spread: more than half the scores identical ⇒ MAD is 0 ⇒ no usable scale.
	flat := make([]float64, 100)
	for i := range flat {
		flat[i] = 0.5
	}

	m := fitNoiseModel(flat, 30)
	if m.ok {
		t.Error("a zero-scale sample must not produce a model")
	}

	if p := m.tailProb(0.99); p != 1 {
		t.Errorf("with no model, every score must read as 'could be noise' (p=1), got %.4f", p)
	}

	if _, ok := m.scoreAtTailProb(0.01); ok {
		t.Error("no model ⇒ no threshold")
	}
}

// TestGateFloorUsesFalseInjectBudget: the adaptive floor knob is now an error RATE, so tightening the
// budget must raise the bar. `adaptive_z = 2.0` could not be reasoned about; "at most 1% of
// irrelevant decisions may clear the gate" can.
func TestGateFloorUsesFalseInjectBudget(t *testing.T) {
	sims := noiseSample(1000, 5)

	cands := make([]scored, len(sims))
	for i, s := range sims {
		cands[i] = scored{d: Decision{ID: "d"}, sim: s}
	}

	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.1") // low, so the adaptive term is what binds

	var prev float64

	for i, alpha := range []string{"0.1", "0.01", "0.001", "0.0001"} {
		c.Set("retrieve.max_false_inject_rate", alpha)

		floor, noise := gateFloorWithNoise(cands, c)
		if !noise.ok {
			t.Fatalf("alpha=%s: noise model should fit", alpha)
		}

		if i > 0 && floor <= prev {
			t.Errorf("alpha=%s: a stricter false-inject budget must RAISE the floor (%.4f after %.4f)",
				alpha, floor, prev)
		}

		prev = floor
	}

	// Disabled (≤0) ⇒ the absolute floor alone, so existing deployments are unchanged until they opt in.
	c.Set("retrieve.max_false_inject_rate", "0")

	if floor, _ := gateFloorWithNoise(cands, c); math.Abs(floor-0.1) > 1e-9 {
		t.Errorf("alpha<=0 must fall back to the absolute floor, got %.4f", floor)
	}
}
