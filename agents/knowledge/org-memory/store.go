package main

import "sync"

// Store is the storage boundary — the whole point of the design's "swap-anytime" storage: the
// engine never cares what is behind it. Phase 0 ships memStore (below) for fast tests and
// sqliteStore for local persistence; Postgres+pgvector is the at-scale swap. Every implementation
// MUST keep the one invariant: SUPERSEDE / QUARANTINE, NEVER DELETE — history is append-only.
type Store interface {
	Put(d Decision)                             // insert or replace by ID
	Get(id string) (Decision, bool)             // fetch one (including superseded/quarantined)
	Active() []Decision                         // non-superseded decisions — recall candidates
	Supersede(oldID, newID string) bool         // mark old superseded + link to new; old row KEPT
	Quarantine(id string) bool                  // stop surfacing; row KEPT
	LinkEdge(a, b, kind string, weight float64) // associative graph edge (for later graph-walk)
	RecordFeedback(f Feedback)                  // append-only feedback event
	Bump(id, signal string, delta int)          // adjust a materialized feedback counter
	Stats(id string) Stats                      // read feedback counters
	SetRelation(child, parent, status string)   // set/replace a scope relation (one parent per child)
	Relations() []Relation                      // all scope relations

	// Meta is a small durable key/value slot for engine state that is neither a decision nor a
	// feedback event — currently the gap work list. Deliberately generic: the alternative was another
	// pair of interface methods per feature, and every implementation paying for all of them.
	GetMeta(key string) string
	SetMeta(key, value string)
}

// memStore is the in-memory Store: concurrency-safe, deterministic, zero-dependency. It makes the
// whole engine unit-testable with no external infra, and is a fine backing for personal/local use.
type memStore struct {
	mu        sync.RWMutex
	meta      map[string]string
	units     map[string]Decision
	order     []string // insertion order → stable Active() output
	stats     map[string]Stats
	edges     []edge
	feedback  []Feedback
	relations map[string]Relation // keyed by child (one parent per child)
}

type edge struct {
	A, B, Kind string
	Weight     float64
}

func newMemStore() *memStore {
	return &memStore{units: map[string]Decision{}, stats: map[string]Stats{}, relations: map[string]Relation{}, meta: map[string]string{}}
}

func (s *memStore) Put(d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.units[d.ID]; !ok {
		s.order = append(s.order, d.ID)
	}

	s.units[d.ID] = d
}

func (s *memStore) Get(id string) (Decision, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	d, ok := s.units[id]

	return d, ok
}

func (s *memStore) Active() []Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Decision, 0, len(s.order))

	for _, id := range s.order {
		if d := s.units[id]; d.SupersededBy == "" {
			out = append(out, d)
		}
	}

	return out
}

// Supersede marks the old decision superseded and links it forward. The old row is retained in full
// — the "overrides are never lost" guarantee.
func (s *memStore) Supersede(oldID, newID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, ok := s.units[oldID]
	if !ok {
		return false
	}

	old.SupersededBy = newID
	s.units[oldID] = old // kept, not deleted

	return true
}

func (s *memStore) Quarantine(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.units[id]
	if !ok {
		return false
	}

	d.Quarantined = true
	s.units[id] = d // kept, just not surfaced

	return true
}

func (s *memStore) LinkEdge(a, b, kind string, weight float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.edges = append(s.edges, edge{a, b, kind, weight})
}

func (s *memStore) RecordFeedback(f Feedback) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.feedback = append(s.feedback, f)
}

func (s *memStore) Bump(id, signal string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.stats[id]

	switch signal {
	case "helpful":
		st.Helpful += delta
	case "used":
		st.Used += delta
	case "not_relevant":
		st.NotRelevant += delta
	case "wrong", "outdated":
		st.Wrong += delta
	}

	s.stats[id] = st
}

func (s *memStore) Stats(id string) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.stats[id]
}

func (s *memStore) SetRelation(child, parent, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if child == "" {
		return
	}

	s.relations[child] = Relation{Child: child, Parent: parent, Status: status}
}

func (s *memStore) Relations() []Relation {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Relation, 0, len(s.relations))
	for _, r := range s.relations {
		out = append(out, r)
	}

	return out
}

// GetMeta / SetMeta — the in-memory store is ephemeral by nature, so these are honest no-ops across
// restarts. Persistence is the SQLite store's job, and the engine treats an empty value as "no
// saved state" either way.
func (s *memStore) GetMeta(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.meta[key]
}

func (s *memStore) SetMeta(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.meta[key] = value
}
