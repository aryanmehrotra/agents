package main

import "testing"

func askItems() []RecalledItem {
	return []RecalledItem{{Decision: Decision{ID: "d-top"}}, {Decision: Decision{ID: "d-2"}}}
}

// confidentDiag is a recall the model is sure about — nothing here should provoke a question except
// the periodic spot check.
func confidentDiag() RecallDiag {
	return RecallDiag{NoiseFitted: true, NoiseP: 1e-15, TopSimilarity: 0.8, Floor: 0.65}
}

// TestAskTargetsUncertainty: the whole point is to spend questions where they teach the most. A recall
// the model is already certain about must not generate one (Lewis & Gale 1994; Settles 2009).
func TestAskTargetsUncertainty(t *testing.T) {
	cfg := NewConfig()

	base := askDecision{items: askItems(), diag: confidentDiag(), health: 1.0, healthN: 100, sampler: &askSampler{}}

	if got := shouldAsk(base, cfg); got != nil {
		t.Errorf("a confident recall off the sampling cadence must not ask; got %q", got.Question)
	}

	for _, tc := range []struct {
		name string
		mut  func(*askDecision)
	}{
		{"weak", func(d *askDecision) { d.weak = true }},
		{"ambiguous", func(d *askDecision) { d.ambiguous = true }},
		{"borderline noise_p", func(d *askDecision) { d.diag.NoiseP = 1e-3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			d.diag = confidentDiag()
			tc.mut(&d)

			got := shouldAsk(d, cfg)
			if got == nil {
				t.Fatal("an uncertain recall must ask")
			}

			if got.DecisionID != "d-top" {
				t.Errorf("the question should be about the top result, got %q", got.DecisionID)
			}
		})
	}
}

// TestAskRateRisesAsConfidenceFalls is the closed loop. The ask rate is not a fixed schedule: it is
// controlled against a target confidence ratio, so a system whose quality is slipping asks MORE
// questions, and quiets down again on its own once the ratio recovers.
func TestAskRateRisesAsConfidenceFalls(t *testing.T) {
	cfg := NewConfig()
	cfg.Set("feedback.sample_every_n", "20")
	cfg.Set("feedback.target_confidence_ratio", "0.9")

	// Count how many of 200 confident recalls get spot-checked at each health level.
	rate := func(health float64) int {
		asked := 0
		sampler := &askSampler{}

		for i := 0; i < 200; i++ {
			d := askDecision{
				items: askItems(), diag: confidentDiag(),
				health: health, healthN: 100, sampler: sampler,
			}
			if shouldAsk(d, cfg) != nil {
				asked++
			}
		}

		return asked
	}

	healthy := rate(0.95) // above target
	slipping := rate(0.60)
	bad := rate(0.20)

	if !(healthy < slipping && slipping < bad) {
		t.Fatalf("ask rate must rise as confidence falls; got healthy=%d slipping=%d bad=%d",
			healthy, slipping, bad)
	}

	if healthy == 0 {
		t.Error("even a healthy system must spot-check occasionally — uncertainty sampling alone is blind to confident-and-wrong")
	}

	// ...and it must never devolve into asking on every single recall.
	if bad > 120 {
		t.Errorf("the loop must stay bounded even at terrible health, asked %d/200", bad)
	}
}

// TestAskIgnoresHealthUntilEnoughSamples: a couple of unlucky recalls must not trigger an
// interrogation. The controller needs evidence before it acts.
func TestAskIgnoresHealthUntilEnoughSamples(t *testing.T) {
	cfg := NewConfig()
	cfg.Set("feedback.health_min_n", "20")

	d := askDecision{items: askItems(), diag: confidentDiag(), health: 0.1, healthN: 3, sampler: &askSampler{}}

	// With too little evidence the rate must not tighten below the baseline cadence.
	if got := shouldAsk(d, cfg); got != nil {
		t.Errorf("health from 3 samples must not drive the ask rate; got %q", got.Question)
	}
}

// TestAskOnNearMissAbstention: a gap in the corpus is invisible to ranking feedback. The only way to
// learn "we should have had a rule for that" is to ask when the abstention was close.
func TestAskOnNearMissAbstention(t *testing.T) {
	cfg := NewConfig()

	near := askDecision{
		items: nil,
		// GapRecorded is the ONLY input the near-miss prompt keys off: it asks iff the gap log
		// actually recorded the question, rather than re-deriving nearness or substance itself.
		diag:   RecallDiag{TopSimilarity: 0.62, Floor: 0.65, GapRecorded: true},
		query:  "kubernetes ingress nginx annotations",
		health: 1.0, healthN: 100, sampler: &askSampler{},
	}

	got := shouldAsk(near, cfg)
	if got == nil {
		t.Fatal("a near-miss abstention should ask whether the corpus has a gap")
	}

	if got.DecisionID != "" {
		t.Error("there is no decision to rate — the question is about the gap, not a result")
	}

	// A far-off-topic abstention is correct and boring: the log declines it, so the prompt does too.
	far := near
	far.diag = RecallDiag{TopSimilarity: 0.20, Floor: 0.65} // GapRecorded false ⇒ the log said no

	if got := shouldAsk(far, cfg); got != nil {
		t.Errorf("a confident abstention must stay silent; got %q", got.Question)
	}
}

// TestAskCanBeSilenced: an assistant that cannot be told to stop asking will be turned off entirely.
func TestAskCanBeSilenced(t *testing.T) {
	cfg := NewConfig()
	cfg.Set("feedback.ask_mode", "off")

	d := askDecision{items: askItems(), diag: confidentDiag(), weak: true, health: 0.1, healthN: 100, sampler: &askSampler{}}

	if got := shouldAsk(d, cfg); got != nil {
		t.Errorf("ask_mode=off must silence everything, got %q", got.Question)
	}
}

// TestRecallHealthIsARollingWindow: the loop must reflect how things are going NOW. A cumulative
// average would let a long healthy history hide a recent regression for thousands of recalls.
func TestRecallHealthIsARollingWindow(t *testing.T) {
	h := newRecallHealth(10)

	if r, n := h.ratio(); r != 1 || n != 0 {
		t.Fatalf("an empty window claims no problem: got %.2f over %d", r, n)
	}

	for i := 0; i < 10; i++ {
		h.record(true)
	}

	if r, n := h.ratio(); r != 1.0 || n != 10 {
		t.Fatalf("10 confident recalls ⇒ 1.0 over 10, got %.2f over %d", r, n)
	}

	// The window turns over: 10 bad recalls must fully displace the good history.
	for i := 0; i < 10; i++ {
		h.record(false)
	}

	if r, _ := h.ratio(); r != 0 {
		t.Fatalf("a full window of failures must read 0.0, got %.2f — old history is leaking through", r)
	}

	for i := 0; i < 5; i++ {
		h.record(true)
	}

	if r, _ := h.ratio(); r != 0.5 {
		t.Fatalf("5 good of the last 10 ⇒ 0.5, got %.2f", r)
	}
}

// TestAskSamplerSurvivesPeriodicTraffic is the regression guard for a bug that live testing found and
// every unit test above missed.
//
// The sampler was `nth % every == 0` over the GLOBAL recall counter. That fails twice over. Recalls
// which return early (the uncertain ones) still advance the counter, so every sampling slot landing
// on one is silently lost. And a modulus ALIASES with any periodicity in the traffic: measured live,
// all three sampling slots in a 40-recall run landed on the same query of a 4-query rotation — the
// one that always returned early as ambiguous — producing ZERO spot checks across 40 recalls while
// health sat at 0.53 against a 0.90 target. The controller was disarmed exactly when it mattered.
//
// The accumulator advances only on recalls that actually reach the sampling decision, so interleaved
// uncertain traffic cannot starve it no matter what period it arrives on.
func TestAskSamplerSurvivesPeriodicTraffic(t *testing.T) {
	cfg := NewConfig()
	cfg.Set("feedback.sample_every_n", "10")
	cfg.Set("feedback.target_confidence_ratio", "0.9")

	sampler := &askSampler{}
	sampled := 0

	// Exactly the shape that broke it: every 4th recall is ambiguous and returns early.
	for i := 0; i < 200; i++ {
		d := askDecision{
			items: askItems(), diag: confidentDiag(),
			ambiguous: i%4 == 3,
			health:    1.0, healthN: 100, sampler: sampler,
		}

		got := shouldAsk(d, cfg)
		if got != nil && !d.ambiguous {
			sampled++
		}
	}

	// 150 confident recalls at 1-in-10 ⇒ ~15. The old modulus produced 0 here.
	if sampled < 12 || sampled > 18 {
		t.Fatalf("expected ~15 spot checks across 150 eligible recalls, got %d", sampled)
	}
}

// TestAskSamplerHitsItsRateUnderDrift: the rate is recomputed from a live health value, so it changes
// underneath the sampler. A modulus with a moving modulus skips; an accumulator carries the remainder.
func TestAskSamplerHitsItsRateUnderDrift(t *testing.T) {
	s := &askSampler{}
	fired := 0

	// Rate drifts every call; over 300 calls the long-run rate should still average out near 1-in-10.
	for i := 0; i < 300; i++ {
		rate := 1.0 / (9 + float64(i%3)) // 1/9 .. 1/11
		if s.tick(rate) {
			fired++
		}
	}

	if fired < 27 || fired > 34 {
		t.Fatalf("expected ~30 fires at ~1-in-10 across 300 ticks, got %d", fired)
	}
}

// TestAskNearMissRespectsTheSubstanceBar: the prompt and the gap log must apply the SAME rule.
//
// They did not, and the mismatch re-created the exact defect POST /gaps/resolve was built to fix.
// Live, the four-word question "is all of that fixed?" triggered the near-miss prompt; the gap log
// correctly refused to record it (1 substance token, bar is 3); and answering the question the
// prompt had just printed returned "that question is not on the gap list". Asking something whose
// answer has nowhere to go is worse than not asking.
func TestAskNearMissRespectsTheSubstanceBar(t *testing.T) {
	cfg := NewConfig()

	nearMiss := RecallDiag{TopSimilarity: 0.62, Floor: 0.65, GapRecorded: true}
	notRecorded := RecallDiag{TopSimilarity: 0.62, Floor: 0.65} // the log refused it

	if got := shouldAsk(askDecision{
		diag: notRecorded, query: "is all of that fixed", health: 1, healthN: 100, sampler: &askSampler{},
	}, cfg); got != nil {
		t.Errorf("must not ask about a question the gap log will refuse to record; got %q", got.Question)
	}

	got := shouldAsk(askDecision{
		diag:   nearMiss,
		query:  "terraform remote state locking backend",
		health: 1, healthN: 100, sampler: &askSampler{},
	}, cfg)
	if got == nil {
		t.Fatal("a substantive near-miss must still be asked about")
	}

	if got.Query != "terraform remote state locking backend" {
		t.Errorf("the prompt must carry the resolvable query, got %q", got.Query)
	}
}
