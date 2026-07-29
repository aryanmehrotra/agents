package main

import (
	"encoding/json"
	"os"
	"sync"
)

// fileStore is memStore with JSON-file persistence — zero-dependency durability for local/personal
// use, behind the same Store interface: it loads on start and snapshots after every mutation, so your
// captured decisions survive a restart. (The at-scale swap is sqlite-vec / Postgres+pgvector; the
// engine never changes.)
type fileStore struct {
	*memStore
	mu   sync.Mutex
	path string
}

type snapshot struct {
	Units      map[string]Decision  `json:"units"`
	Embeddings map[string][]float32 `json:"embeddings"` // kept separately: Decision.Embedding is json:"-"
	Order      []string             `json:"order"`
	Stats      map[string]Stats     `json:"stats"`
	Edges      []edge               `json:"edges"`
	Feedback   []Feedback           `json:"feedback"`
}

func newFileStore(path string) (*fileStore, error) {
	fs := &fileStore{memStore: newMemStore(), path: path}
	if err := fs.load(); err != nil {
		return nil, err
	}

	return fs, nil
}

func (fs *fileStore) load() error {
	b, err := os.ReadFile(fs.path)
	if os.IsNotExist(err) || len(b) == 0 {
		return nil
	}

	if err != nil {
		return err
	}

	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	fs.memStore.mu.Lock()
	defer fs.memStore.mu.Unlock()

	if s.Units != nil {
		fs.memStore.units = s.Units
	}

	for id, d := range fs.memStore.units { // reattach embeddings (dropped by json:"-")
		if v, ok := s.Embeddings[id]; ok {
			d.Embedding = v
			fs.memStore.units[id] = d
		}
	}

	fs.memStore.order = s.Order

	if s.Stats != nil {
		fs.memStore.stats = s.Stats
	}

	fs.memStore.edges = s.Edges
	fs.memStore.feedback = s.Feedback

	return nil
}

func (fs *fileStore) save() {
	fs.memStore.mu.RLock()
	s := snapshot{
		Units:      fs.memStore.units,
		Embeddings: map[string][]float32{},
		Order:      fs.memStore.order,
		Stats:      fs.memStore.stats,
		Edges:      fs.memStore.edges,
		Feedback:   fs.memStore.feedback,
	}

	for id, d := range fs.memStore.units {
		if len(d.Embedding) > 0 {
			s.Embeddings[id] = d.Embedding
		}
	}
	fs.memStore.mu.RUnlock()

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	tmp := fs.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, fs.path) // atomic replace
	}
}

// Mutating methods persist after delegating to the in-memory core.
func (fs *fileStore) Put(d Decision)                     { fs.memStore.Put(d); fs.save() }
func (fs *fileStore) Quarantine(id string) bool          { ok := fs.memStore.Quarantine(id); fs.save(); return ok }
func (fs *fileStore) LinkEdge(a, b, k string, w float64) { fs.memStore.LinkEdge(a, b, k, w); fs.save() }
func (fs *fileStore) RecordFeedback(f Feedback)          { fs.memStore.RecordFeedback(f); fs.save() }
func (fs *fileStore) Bump(id, sig string, d int)         { fs.memStore.Bump(id, sig, d); fs.save() }

func (fs *fileStore) Supersede(oldID, newID string) bool {
	ok := fs.memStore.Supersede(oldID, newID)
	fs.save()

	return ok
}
