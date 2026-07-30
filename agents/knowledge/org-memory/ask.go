package main

import (
	"fmt"
	"sync"
)

// Asking for feedback, instead of waiting for it — governed by a measured confidence ratio.
//
// THE PROBLEM. The feedback loop has existed since Phase 0 and is almost entirely unused: 151 recalls
// have produced 1 label. That is not apathy, it is design — nothing ever prompts anyone, so labelling
// requires remembering the endpoint exists and deciding, unprompted, to use it. The cost is concrete:
// the ~13 ranking constants that are still chosen rather than fitted can only be fitted from
// interaction data (counterfactual/unbiased learning-to-rank, Joachims et al. 2017), and there is
// nowhere near enough. The blocker on those knobs was never research. It is that the system never asks.
//
// WHICH RECALLS. Not all of them — that is what makes feedback prompts annoying enough to be switched
// off, and most labels would be worthless anyway: a recall at noise_p = 2e-15 is one the system is
// already certain about, and confirming it teaches nothing. Ask where the model is LEAST certain,
// because those labels carry the most information per unit of human patience (uncertainty sampling —
// Lewis & Gale, SIGIR 1994; Settles, "Active Learning Literature Survey", 2009). This system is
// unusually well set up for it: `noise_p` is already a calibrated probability, so "least certain" is
// a measured quantity rather than a heuristic.
//
// HOW MANY — THE CLOSED LOOP. The ask rate is not a fixed schedule. The engine tracks, over a rolling
// window, what FRACTION of recalls came back confident, and compares it to a target. Above target the
// system is healthy and questions taper off to a trickle; below target something has degraded — drift,
// a batch of noisy captures, a corpus outgrowing its floor — and questions ramp back up automatically,
// concentrating labelling effort exactly when and where quality is slipping. That is the difference
// between a survey and a controller: the loop holds the ratio up rather than sampling at a rate
// someone guessed once.
//
// AND THE BLIND SPOT. Uncertainty sampling never asks about cases the model is confident on, so on its
// own it can NEVER discover CONFIDENT-AND-WRONG — which is this system's most dangerous failure mode
// (the negation results come back with weak:false). A slice of confident recalls is sampled too, and
// the shortfall against the target is what decides how thick that slice is.

// AskPrompt is the question attached to a recall, or nil. Deliberately tiny: one line, one decision,
// three options. A feedback request that needs reading is a feedback request that gets ignored.
type AskPrompt struct {
	Question   string `json:"question"`
	DecisionID string `json:"decision_id,omitempty"`
	// Query is set only for near-miss abstention questions, whose answer goes to POST /gaps/resolve.
	// There is no decision to rate, so the question is identified by what was asked.
	Query   string   `json:"query,omitempty"`
	Options []string `json:"options"`
	Reason  string   `json:"reason"` // why THIS recall was picked — keeps the policy legible
}

// recallHealth is a rolling window of "was this recall confident?", the signal the ask-rate loop
// controls against. A window rather than a lifetime counter, because the question is "how are we
// doing NOW" — a system that was healthy for its first thousand recalls and has since drifted should
// start asking again, and a cumulative average would hide that for a long time.
type recallHealth struct {
	mu     sync.Mutex
	window []bool
	next   int
	filled bool
}

func newRecallHealth(size int) *recallHealth {
	if size < 1 {
		size = 1
	}

	return &recallHealth{window: make([]bool, size)}
}

func (h *recallHealth) record(confident bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.window[h.next] = confident
	h.next = (h.next + 1) % len(h.window)

	if h.next == 0 {
		h.filled = true
	}
}

// ratio returns the fraction of recent recalls that were confident, and the sample size behind it.
// An unfilled window reports what it has; the caller decides whether that is enough to act on.
func (h *recallHealth) ratio() (float64, int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := len(h.window)
	if !h.filled {
		n = h.next
	}

	if n == 0 {
		return 1, 0 // no evidence of a problem yet
	}

	confident := 0

	for i := 0; i < n; i++ {
		if h.window[i] {
			confident++
		}
	}

	return float64(confident) / float64(n), n
}

// askSampler decides which CONFIDENT recalls get spot-checked, at a target rate.
//
// It replaces an `nth % every == 0` test on the global recall counter, which testing showed to be
// broken in two compounding ways. First, a modulus over a counter that includes recalls which return
// early (the uncertain ones) is systematically starved: every sampling slot that lands on an
// uncertain recall is silently lost, so the effective rate is far below the configured one. Second,
// it ALIASES — measured live, all three sampling slots in a 40-recall run landed on the same query in
// a 4-query rotation, and that query happened to be the one that always returned early as ambiguous.
// Result: zero spot checks across 40 recalls while health sat at 0.53 against a 0.90 target, i.e. the
// controller was fully disarmed exactly when it was most needed. A modulus assumes traffic has no
// periodicity; real traffic does.
//
// An accumulator has neither failure. It advances only on recalls that actually reach the sampling
// decision, and it carries the remainder, so the long-run rate is exactly the requested one even as
// the rate changes underneath it. Deterministic, so a test can pin it — no RNG.
type askSampler struct {
	mu  sync.Mutex
	acc float64
}

// tick advances the sampler by `rate` (asks per eligible recall) and reports whether to ask now.
func (s *askSampler) tick(rate float64) bool {
	if rate <= 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.acc += rate
	if s.acc >= 1 {
		s.acc--

		return true
	}

	return false
}

// askDecision bundles the policy inputs so the rule below reads as one statement.
type askDecision struct {
	items     []RecalledItem
	diag      RecallDiag
	weak      bool
	ambiguous bool
	health    float64
	healthN   int
	sampler   *askSampler
	query     string // the question asked — needed to answer a near-miss prompt
	silenced  bool   // a human already answered "correctly silent" for this question
}

// shouldAsk decides whether to attach a feedback question to this recall.
func shouldAsk(d askDecision, cfg *Config, chain ...string) *AskPrompt {
	if cfg.Str("feedback.ask_mode", "auto", chain...) == "off" {
		return nil
	}

	target := cfg.F("feedback.target_confidence_ratio", 0.9, chain...)
	minN := cfg.I("feedback.health_min_n", 20, chain...)

	// An abstention is worth asking about when it was CLOSE: "should I have known this?" is the only
	// way to learn about a GAP in the corpus, which no amount of ranking feedback can ever reveal.
	if len(d.items) == 0 {
		// A human who already said "correctly silent" must not be asked again. Deleting the gap entry
		// was not enough — this prompt fires on the abstention itself, so the next identical query
		// re-asked. An answered question that keeps getting re-asked teaches people to ignore prompts.
		if d.silenced {
			return nil
		}

		// Ask iff the gap log ACTUALLY RECORDED this question. Not "iff it was near enough", not "iff
		// it has enough substance" — those are re-derivations, and re-deriving the rule is what caused
		// this defect three separate times: a prompt whose printed payload its own endpoint rejects.
		// Substance and nearness were unified one at a time and a third pair of knobs
		// (feedback.ask_near_miss 0.05 vs gaps.near_miss 0.08) was still free to drift; the defaults
		// only agreed by coincidence. There is now one rule, in one place, and the prompt reports its
		// verdict rather than guessing at it.
		if d.diag.GapRecorded {
			return &AskPrompt{
				Question: "Found nothing for that — but something came close. Should I have known it?",
				// Carries the query, because this answer goes to POST /gaps/resolve rather than
				// /feedback: an abstention has no decision_id to attach a verdict to.
				Query:   d.query,
				Options: []string{"yes — we have a rule for this", "no — correctly silent"},
				Reason:  fmt.Sprintf("near-miss abstention (best %.3f vs floor %.3f)", d.diag.TopSimilarity, d.diag.Floor),
			}
		}

		return nil
	}

	top := d.items[0].Decision.ID

	// UNCERTAIN — the model itself says it is unsure. Always worth a question.
	switch {
	case d.weak:
		return ask(top, "Low confidence on that one — was it right?")
	case d.ambiguous:
		return ask(top, "Several results looked equally good — was the first one right?")
	case d.diag.NoiseFitted && d.diag.NoiseP > cfg.F("feedback.ask_noise_p", 1e-6, chain...):
		return ask(top, fmt.Sprintf("Borderline match (%.0e chance it's noise) — was it right?", d.diag.NoiseP))
	}

	// CONFIDENT — sample a slice, at a rate set by how far health has fallen below target.
	//
	// At or above target: sample at the baseline rate (rare — a background check).
	// Below target: tighten proportionally, so a system whose confidence is slipping asks more often,
	// and stops asking again on its own once the ratio recovers. Floored at 2 so the loop cannot turn
	// into a question on every single recall no matter how bad things get.
	every := float64(cfg.I("feedback.sample_every_n", 20, chain...))
	if every <= 0 || d.sampler == nil {
		return nil
	}

	if d.healthN >= minN && d.health < target && target > 0 {
		every *= d.health / target
		if every < 2 {
			every = 2
		}
	}

	if d.sampler.tick(1 / every) {
		q := "Spot check — was that actually useful?"
		if d.healthN >= minN && d.health < target {
			q = fmt.Sprintf("Confidence is at %.0f%% (target %.0f%%) — was that one useful?", d.health*100, target*100)
		}

		return ask(top, q)
	}

	return nil
}

func ask(id, question string) *AskPrompt {
	return &AskPrompt{
		Question:   question,
		DecisionID: id,
		// EXACT API signal values — these are copied straight into POST /feedback, so they must not
		// carry explanatory suffixes. The taxonomy is taught alongside them by the renderer
		// (integrations/orgmem-recall.sh), because a user reaching for "wrong" to mean "that did not
		// answer me" retires a true decision after two marks. See the Feedback doc in types.go.
		Options: []string{"helpful", "not_relevant", "wrong"},
		Reason:  question,
	}
}
