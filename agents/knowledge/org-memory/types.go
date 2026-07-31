// Package main is the org-memory engine (Phase 0). It captures the WHY behind decisions,
// stores them behind a swappable Store, retrieves the few that matter for a context, advises
// them back into an AI agent (or a human), and learns from feedback. Phase 0 is advisory-only:
// it surfaces prior decisions as context — it never decides truth on its own. Correctness
// scoring and the self-learning loop arrive in later phases; the boundaries here are built so
// those bolt on without changing this code.
//
// Design law (repo-wide): "Model proposes, Go disposes." The LLM drafts a decision's text; every
// deterministic thing here — the content-hash ID, scope matching, ranking, the precision floor,
// supersede-never-delete, and the feedback effects — is plain Go, so nothing is asserted without
// provenance and nothing is hardcoded (every knob is Config, live-tunable).
package main

import "time"

// Decision is one captured unit of "why". Advisory in Phase 0.
type Decision struct {
	ID           string    `json:"id"`                   // content hash of what+scope (dedupe/reinforce)
	What         string    `json:"what"`                 // the decision / rule
	Why          string    `json:"why,omitempty"`        // the rationale (the valuable part)
	Scope        []string  `json:"scope"`                // e.g. ["repo:x","service:y"]; empty = global
	Provenance   string    `json:"provenance,omitempty"` // who/role — a relevance/authority hint
	Source       string    `json:"source,omitempty"`     // source span / link (grounding)
	Quarantined  bool      `json:"quarantined"`          // stop surfacing (kept, not deleted)
	Supersedes   string    `json:"supersedes,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"` // set on the OLD row; it is KEPT
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	Embedding    []float32 `json:"-"`
}

// Stats are materialized feedback counters used to nudge ranking. Relevance signal only —
// never treated as correctness (the honesty rule).
type Stats struct {
	Helpful     int `json:"helpful"`
	NotRelevant int `json:"not_relevant"`
	Wrong       int `json:"wrong"`
	Used        int `json:"used"`
}

// RecalledItem is a ranked decision plus its rendered advice line.
type RecalledItem struct {
	Decision   Decision `json:"decision"`
	Score      float64  `json:"score"`      // composite rank score (relevance+recency+importance+retention+feedback)
	Similarity float64  `json:"similarity"` // raw cosine relevance to the query, in [-1,1] — the semantic match
	Guidance   string   `json:"guidance"`
}

// RecallDiag makes an abstention MEASURABLE. Returning an empty list is the correct behaviour when
// nothing is relevant enough, but it says nothing about how close the call was — and a caller that
// reports the top similarity of an empty list gets 0.0, which reads like "the corpus contains nothing
// remotely related" when the truth may be "the best match scored 0.64 against a 0.65 bar". That
// distinction is the difference between a calibration measurement and an artifact, so the numbers are
// carried out of Recall explicitly rather than reconstructed by the caller. Selective prediction is
// scored on the risk-coverage curve (El-Yaniv & Wiener, JMLR 2010) — an abstention you cannot measure
// cannot be placed on that curve at all, so the measurement is a precondition for the honesty claim,
// not a diagnostic extra. See RESEARCH.md §10c.
type RecallDiag struct {
	TopSimilarity float64 `json:"top_similarity"` // best cosine among surfaceable decisions BEFORE the gate
	Floor         float64 `json:"floor"`          // the effective floor applied (absolute or adaptive)
	Candidates    int     `json:"candidates"`     // how many decisions were eligible to be scored

	// NoiseP is the probability that an IRRELEVANT decision would score as high as the best one did,
	// under the noise model fitted to this query's own score distribution (see scoredist.go). It is
	// the calibrated replacement for the hand-picked confidence margins: a cosine of 0.8 means nothing
	// on its own, but "there is a 1-in-10,000 chance noise produces this" is comparable across
	// corpora, embedding models and queries. 1.0 means no model could be fitted, i.e. claim nothing.
	NoiseP float64 `json:"noise_p"`
	// NoiseFitted reports whether the model is usable at all — a sample too small, or a degenerate
	// spread, must read as "unknown" rather than silently producing a confident-looking probability.
	NoiseFitted bool `json:"noise_fitted"`

	// Scan coverage. An abstention ("nothing was relevant enough") and a truncated scan ("we ran out
	// of time before looking at everything") are completely different claims, and a system whose
	// product is knowing when it does not know must never let the second masquerade as the first.
	Scanned        int  `json:"scanned"`
	CorpusSize     int  `json:"corpus_size"`
	BudgetExceeded bool `json:"budget_exceeded"`

	// GapRecorded reports whether this abstention actually landed on the capture work list. The
	// near-miss PROMPT keys off this rather than re-deriving "was it close enough", because deriving
	// the same rule twice is how the prompt ended up asking questions its own endpoint rejects —
	// three separate times, each with a different pair of knobs drifting apart.
	GapRecorded bool `json:"gap_recorded"`
}

// Relation is a parent→child link in the scope hierarchy, with a confirmation status. Confirmed
// relations (set by a human, or a proposal accepted yes/no) define the tree that recall inherits
// along — a query in a child scope also surfaces its ancestors' decisions. Persisted in the backend.
type Relation struct {
	Child  string `json:"child"`  // e.g. "service:aggregator"
	Parent string `json:"parent"` // e.g. "layer:backend"
	Status string `json:"status"` // confirmed | proposed | rejected
}

// Feedback is a reaction to a surfaced decision — the improvement signal.
//
// THE SIGNALS MEAN DIFFERENT KINDS OF THING, and picking the wrong one has teeth:
//
//	helpful / used   the decision answered the question          (relation — nudges ranking up)
//	not_relevant     poor answer to THIS question; the decision   (relation — nudges ranking down;
//	                 may still be perfectly true                   must NOT affect the decision itself)
//	wrong / outdated the decision is FALSE or stale               (property — erodes retrievability,
//	                                                               and auto-quarantines at N marks)
//
// Relevance is a RELATION between a document and an information need, never a property the document
// owns (Saracevic, JASIST 2007). So `not_relevant` can never justify retiring or decaying a decision:
// a bad match for one phrasing says nothing about whether the rule is true. `wrong` is the opposite —
// a claim about the decision itself — and is the only signal permitted to remove it from memory.
//
// This is a live footgun, discovered by tripping it: marking `wrong` twice on a correct
// rate-limiting decision (because it was the wrong ANSWER to a fail-closed question) quarantined a
// true rule. `not_relevant` was the right signal and would have been harmless.
//
// `Context` is the query scope a relation-signal belongs to. It was declared in Phase 0, accepted by
// the API, and never read — so `not_relevant` demoted globally. It is now honoured: relevance
// judgements are counted per (decision, information-need), and ranking demotes only for the question
// the judgement was actually made about. When no context is given the engine falls back to the query
// that surfaced the decision; when neither is available the judgement is still counted for reporting
// but applied to no ranking at all, because a relation with no second term cannot be acted on.
type Feedback struct {
	DecisionID string   `json:"decision_id"`
	Context    []string `json:"context,omitempty"`
	Signal     string   `json:"signal"` // helpful | used | not_relevant | wrong | outdated
	By         string   `json:"by,omitempty"`
	Note       string   `json:"note,omitempty"`
}
