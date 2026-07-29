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
	ID           string    `json:"id"`             // content hash of what+scope (dedupe/reinforce)
	What         string    `json:"what"`           // the decision / rule
	Why          string    `json:"why,omitempty"`  // the rationale (the valuable part)
	Scope        []string  `json:"scope"`          // e.g. ["repo:x","service:y"]; empty = global
	Provenance   string    `json:"provenance,omitempty"` // who/role — a relevance/authority hint
	Source       string    `json:"source,omitempty"`     // source span / link (grounding)
	Quarantined  bool      `json:"quarantined"`    // stop surfacing (kept, not deleted)
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
	Decision Decision `json:"decision"`
	Score    float64  `json:"score"`
	Guidance string   `json:"guidance"`
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
type Feedback struct {
	DecisionID string   `json:"decision_id"`
	Context    []string `json:"context,omitempty"`
	Signal     string   `json:"signal"` // helpful | used | not_relevant | wrong | outdated
	By         string   `json:"by,omitempty"`
	Note       string   `json:"note,omitempty"`
}
