package main

import (
	"math"
	"sort"
)

// Score-distribution modelling: turn "how relevant is this?" into a probability, so the thresholds
// that decide whether to speak stop being magic numbers.
//
// WHY THIS FILE EXISTS. The confidence machinery was a stack of asserted constants — a 0.10
// weak margin, a 0.03 runner-up margin, an adaptive floor at "mean + 2σ". None of them could be
// justified, none had a unit, and none transferred across corpora. For a system whose entire claim is
// calibrated honesty, an unjustifiable threshold is the same defect class as a masked similarity
// score: a number that reads as a measurement and is not.
//
// The established treatment is to model the score distribution itself. For any query, the candidate
// scores are a MIXTURE: a large non-relevant bulk plus a small relevant tail (Manmatha, Rath & Feng,
// SIGIR 2001; Arampatzis & Robertson, "Modeling score distributions in information retrieval",
// Information Retrieval 2011). Fit the NON-RELEVANT component and every threshold becomes a
// statement about it: "admit a score only if it is too extreme to be noise." That is exactly the
// signal-to-noise normalisation of Arampatzis & Kamps (CIKM 2009), which is also — unknowingly — what
// this system's boot-time calibration probe was already doing, and the cutoff-selection framing of
// Arampatzis, Kamps & Robertson ("Where to stop reading a ranked list?", SIGIR 2009).
//
// WHY ROBUST STATISTICS, NOT mean+z·σ. The previous adaptive floor estimated mean and σ over ALL
// candidate similarities — including the relevant ones it was meant to admit. Relevant documents live
// in the right tail, so they inflate both moments and push the floor UP. The floor therefore rose
// precisely BECAUSE the query had good answers, and a query with many relevant decisions was gated
// harder than one with few. Measured on a simulated corpus (1600 noise + k relevant):
//
//	relevant docs:      0        3       20       60
//	mean + 2σ:     0.5716   0.5752   0.5990   0.6407   ← drifts up with the answers
//	median + 2·MAD:0.5744   0.5747   0.5774   0.5829   ← stays put
//
// The median and MAD have a breakdown point of 50% — they are unaffected until over half the sample
// is "contaminated" — which is the textbook remedy for exactly this shape (Rousseeuw & Croux,
// "Alternatives to the Median Absolute Deviation", JASA 1993; Huber, Robust Statistics). Since the
// relevant tail is by construction a small minority, robust estimation measures the noise even though
// the noise is mixed with signal.

// normalIQRHalf is Φ⁻¹(0.75): for normally-distributed data, median − Q1 = 0.6745·σ. Dividing the
// lower semi-interquartile range by it yields a consistent estimate of σ.
const normalIQRHalf = 0.6744897501960817

// noiseModel is a robust estimate of the NON-RELEVANT score distribution for a single query.
type noiseModel struct {
	center float64 // median — the typical score of an irrelevant decision
	scale  float64 // MAD-based σ estimate
	n      int
	ok     bool // false when the sample is too small to say anything honest
}

// fitNoiseModel estimates the noise distribution from a query's candidate scores.
//
// minN exists because a scale estimate from a handful of points is itself noise. The relative
// standard error of a σ estimate is ~1/√(2(n−1)) — 27% at n=8, 13% at n=30 — and MAD is only ~37%
// efficient at the normal, so it needs MORE samples than that, not fewer. A threshold derived from a
// 27%-uncertain scale is not a calibrated threshold, so below minN we decline to adapt and say so.
func fitNoiseModel(sims []float64, minN int) noiseModel {
	if len(sims) < minN || len(sims) == 0 {
		return noiseModel{n: len(sims)}
	}

	xs := append([]float64(nil), sims...)
	sort.Float64s(xs)

	center := median(xs)

	// ONE-SIDED scale, from the LOWER half only: σ̂ = (Q2 − Q1)/Φ⁻¹(0.75).
	//
	// The contamination here is not symmetric — relevant documents are exclusively HIGH scores, so
	// they pollute only the right tail. The MAD averages absolute deviations on BOTH sides, so the
	// inflated right tail still leaks into the estimate: measured on 1600 noise + 150 relevant, MAD
	// drifted 11.6% and moved the derived threshold by 0.024. Estimating the spread from the median
	// down to the first quartile touches no score above the median, so right-tail contamination is
	// invisible to it until it exceeds ~25% of the sample — far past anything a real query produces.
	//
	// This is the "truncated score distribution" idea made concrete: fit the non-relevant component on
	// the part of the range where it is the only component (Arampatzis, Kamps & Robertson, "Where to
	// stop reading a ranked list? Threshold optimization using truncated score distributions", SIGIR
	// 2009). Robust-scale foundations: Rousseeuw & Croux, JASA 1993; Huber, Robust Statistics.
	q1 := quantile(xs, 0.25)

	scale := (center - q1) / normalIQRHalf
	if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
		// A degenerate spread (>half the scores identical) means the corpus cannot discriminate for
		// this query at all. Report unusable rather than manufacture a threshold from a zero scale.
		return noiseModel{center: center, n: len(xs)}
	}

	return noiseModel{center: center, scale: scale, n: len(xs), ok: true}
}

// median of an already-sorted slice.
func median(sorted []float64) float64 { return quantile(sorted, 0.5) }

// quantile returns the p-quantile of an already-sorted slice by linear interpolation.
func quantile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}

	if n == 1 {
		return sorted[0]
	}

	pos := p * float64(n-1)

	lo := int(math.Floor(pos))
	if lo < 0 {
		lo = 0
	}

	hi := lo + 1
	if hi > n-1 {
		return sorted[n-1]
	}

	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// tailProb returns P(score ≥ s) under the fitted noise model — the probability that an IRRELEVANT
// decision would score at least this high by chance. This is the number the whole file exists to
// produce: it has a unit, it is comparable across corpora and embedding models, and a threshold on it
// means something a human can argue with ("inject only if there is under a 1% chance this is noise").
func (m noiseModel) tailProb(s float64) float64 {
	if !m.ok {
		return 1 // no model ⇒ we cannot rule out noise ⇒ claim nothing
	}

	// Upper tail of the normal: P(X ≥ s) = ½·erfc(z/√2).
	return 0.5 * math.Erfc((s-m.center)/(m.scale*math.Sqrt2))
}

// scoreAtTailProb inverts tailProb: the score a decision must beat for its "could be noise"
// probability to fall to alpha. This is how a false-inject BUDGET becomes a cosine threshold —
// the knob an operator sets is the error rate they will tolerate, not an opaque similarity number.
func (m noiseModel) scoreAtTailProb(alpha float64) (float64, bool) {
	if !m.ok || alpha <= 0 || alpha >= 1 {
		return 0, false
	}

	// z = Φ⁻¹(1−α) via the inverse error function.
	z := math.Sqrt2 * math.Erfinv(1-2*alpha)

	return m.center + z*m.scale, true
}
