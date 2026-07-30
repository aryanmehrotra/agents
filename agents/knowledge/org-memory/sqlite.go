package main

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO) — already a repo dependency
)

// sqliteStore is an embedded, pure-Go, single-file Store: durable local persistence with ZERO infra
// (no server, no Docker), behind the same Store interface. Decisions live in SQL; embeddings are
// stored as float32 BLOBs and recall does exact brute-force cosine in Go (correct at local scale).
// The at-scale swap (a sqlite-vec index, or Postgres+pgvector) keeps this same interface — the engine
// never changes. MaxOpenConns(1) + busy_timeout serialises access so concurrent use stays consistent
// without "database is locked" errors.
type sqliteStore struct {
	db *sql.DB
}

func newSQLiteStore(path string) (*sqliteStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)

	s := &sqliteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()

		return nil, err
	}

	return s, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

func (s *sqliteStore) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS decisions(
  id TEXT PRIMARY KEY, what TEXT, why TEXT, scope TEXT, provenance TEXT, source TEXT,
  quarantined INTEGER DEFAULT 0, supersedes TEXT, superseded_by TEXT,
  created TEXT, updated TEXT, embedding BLOB, ord INTEGER);
CREATE TABLE IF NOT EXISTS stats(
  id TEXT PRIMARY KEY, helpful INT DEFAULT 0, not_relevant INT DEFAULT 0,
  wrong INT DEFAULT 0, used INT DEFAULT 0);
CREATE TABLE IF NOT EXISTS edges(a TEXT, b TEXT, kind TEXT, weight REAL);
CREATE TABLE IF NOT EXISTS feedback(
  seq INTEGER PRIMARY KEY AUTOINCREMENT, decision_id TEXT, signal TEXT, by_actor TEXT, note TEXT, ts TEXT);
CREATE TABLE IF NOT EXISTS relations(child TEXT PRIMARY KEY, parent TEXT, status TEXT);
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);`)

	return err
}

func (s *sqliteStore) Put(d Decision) {
	scope, _ := json.Marshal(d.Scope)

	var ord int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(ord),0)+1 FROM decisions`).Scan(&ord)

	_, _ = s.db.Exec(`
INSERT INTO decisions(id,what,why,scope,provenance,source,quarantined,supersedes,superseded_by,created,updated,embedding,ord)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET what=excluded.what, why=excluded.why, scope=excluded.scope,
  provenance=excluded.provenance, source=excluded.source, quarantined=excluded.quarantined,
  supersedes=excluded.supersedes, superseded_by=excluded.superseded_by, updated=excluded.updated,
  embedding=excluded.embedding`,
		d.ID, d.What, d.Why, string(scope), d.Provenance, d.Source, b2i(d.Quarantined),
		d.Supersedes, d.SupersededBy, tstr(d.Created), tstr(d.Updated), encodeVec(d.Embedding), ord)
}

const decisionCols = `id,what,why,scope,provenance,source,quarantined,supersedes,superseded_by,created,updated,embedding`

func (s *sqliteStore) Get(id string) (Decision, bool) {
	d, err := scanDecision(s.db.QueryRow(`SELECT `+decisionCols+` FROM decisions WHERE id=?`, id))
	if err != nil {
		return Decision{}, false
	}

	return d, true
}

func (s *sqliteStore) Active() []Decision {
	rows, err := s.db.Query(`SELECT ` + decisionCols +
		` FROM decisions WHERE superseded_by='' OR superseded_by IS NULL ORDER BY ord`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Decision

	for rows.Next() {
		if d, err := scanDecision(rows); err == nil {
			out = append(out, d)
		}
	}

	return out
}

func (s *sqliteStore) Supersede(oldID, newID string) bool {
	res, err := s.db.Exec(`UPDATE decisions SET superseded_by=? WHERE id=?`, newID, oldID)
	if err != nil {
		return false
	}

	n, _ := res.RowsAffected()

	return n > 0
}

func (s *sqliteStore) Quarantine(id string) bool {
	res, err := s.db.Exec(`UPDATE decisions SET quarantined=1 WHERE id=?`, id)
	if err != nil {
		return false
	}

	n, _ := res.RowsAffected()

	return n > 0
}

func (s *sqliteStore) LinkEdge(a, b, kind string, weight float64) {
	_, _ = s.db.Exec(`INSERT INTO edges(a,b,kind,weight) VALUES(?,?,?,?)`, a, b, kind, weight)
}

func (s *sqliteStore) RecordFeedback(f Feedback) {
	_, _ = s.db.Exec(`INSERT INTO feedback(decision_id,signal,by_actor,note,ts) VALUES(?,?,?,?,?)`,
		f.DecisionID, f.Signal, f.By, f.Note, time.Now().UTC().Format(time.RFC3339Nano))
}

// bumpCol whitelists signal→column (also prevents SQL injection via the signal string).
var bumpCol = map[string]string{
	"helpful": "helpful", "used": "used", "not_relevant": "not_relevant",
	"wrong": "wrong", "outdated": "wrong",
}

func (s *sqliteStore) Bump(id, signal string, delta int) {
	col, ok := bumpCol[signal]
	if !ok {
		return
	}

	_, _ = s.db.Exec(`INSERT INTO stats(id,`+col+`) VALUES(?,?) `+
		`ON CONFLICT(id) DO UPDATE SET `+col+`=`+col+`+?`, id, delta, delta)
}

func (s *sqliteStore) Stats(id string) Stats {
	var st Stats
	_ = s.db.QueryRow(`SELECT helpful,not_relevant,wrong,used FROM stats WHERE id=?`, id).
		Scan(&st.Helpful, &st.NotRelevant, &st.Wrong, &st.Used)

	return st
}

func (s *sqliteStore) SetRelation(child, parent, status string) {
	if child == "" {
		return
	}

	_, _ = s.db.Exec(`INSERT INTO relations(child,parent,status) VALUES(?,?,?) `+
		`ON CONFLICT(child) DO UPDATE SET parent=excluded.parent, status=excluded.status`, child, parent, status)
}

func (s *sqliteStore) Relations() []Relation {
	rows, err := s.db.Query(`SELECT child,parent,status FROM relations`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Relation

	for rows.Next() {
		var r Relation
		if rows.Scan(&r.Child, &r.Parent, &r.Status) == nil {
			out = append(out, r)
		}
	}

	return out
}

// --- helpers ---

type rowScanner interface{ Scan(...any) error }

func scanDecision(sc rowScanner) (Decision, error) {
	var (
		d                Decision
		scope            string
		quar             int
		blob             []byte
		created, updated string
	)

	if err := sc.Scan(&d.ID, &d.What, &d.Why, &scope, &d.Provenance, &d.Source,
		&quar, &d.Supersedes, &d.SupersededBy, &created, &updated, &blob); err != nil {
		return Decision{}, err
	}

	_ = json.Unmarshal([]byte(scope), &d.Scope)
	d.Quarantined = quar != 0
	d.Embedding = decodeVec(blob)
	d.Created = tparse(created)
	d.Updated = tparse(updated)

	return d, nil
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}

	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}

	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}

	return 0
}

func tstr(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.UTC().Format(time.RFC3339Nano)
}

func tparse(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	t, _ := time.Parse(time.RFC3339Nano, s)

	return t
}

// GetMeta returns durable engine state, or "" when nothing is stored.
func (s *sqliteStore) GetMeta(key string) string {
	var v string

	_ = s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&v)

	return v
}

// SetMeta writes durable engine state. Whole-value upsert: the payloads are small and bounded, and a
// snapshot cannot half-apply the way an incremental update can.
func (s *sqliteStore) SetMeta(key, value string) {
	_, _ = s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
}
