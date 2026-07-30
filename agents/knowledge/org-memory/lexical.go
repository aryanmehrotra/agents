package main

import (
	"math"
	"strings"
	"sync"
)

// Lexical (BM25) retrieval, fused with the dense signal.
//
// WHY. Both of this system's known retrieval failures have one cause: a bi-encoder compresses a whole
// decision into a single vector, and that vector encodes TOPIC far more strongly than the particular
// tokens that distinguish one decision from another.
//
//   - ENTITY CONFLATION — "which database is the source of truth for order data" surfaces a decision
//     about `order_events` above the one about `orders`. Both are true, both are
//     about "source of truth for a storage table", and the embedding cannot tell the two identifiers
//     apart because they occupy almost the same topical region.
//   - NEGATION BLINDNESS — "should we fail closed" returns the "fail OPEN" decision. `open` and
//     `closed` are one token apart in a sentence that is otherwise identical.
//
// In both cases the distinguishing evidence is an EXACT TOKEN, which is exactly what dense retrieval
// discards and exact-match retrieval is built on. This is a well-measured property, not a local
// quirk: BEIR found dense retrievers underperform BM25 out-of-distribution and specifically on
// entity-heavy queries (Thakur et al., NeurIPS Datasets & Benchmarks 2021), which is why production
// systems run hybrid rather than choosing. BM25 itself: Robertson & Zaragoza, "The Probabilistic
// Relevance Framework: BM25 and Beyond" (FnTIR 2009).
//
// FUSION. The two scores are not comparable — a cosine lives in [-1,1] and a BM25 score is unbounded
// and corpus-dependent — so they are fused by RANK, not by value, using Reciprocal Rank Fusion
// (Cormack, Clarke & Buettcher, SIGIR 2009). RRF is scale-immune by construction, which is the whole
// reason to prefer it here: no normalisation constant to tune, and no possibility of one signal
// dominating because its units happen to be larger. The same argument the ranker already makes for
// keeping priors bounded applies to keeping the lexical signal from swamping the dense one.

// bm25Index is an inverted index over the surfaceable corpus. Rebuilt when the corpus changes;
// cheap enough at this scale that incremental maintenance would be complexity without benefit.
type bm25Index struct {
	mu sync.RWMutex

	df       map[string]int // term → number of documents containing it
	postings map[string][]posting
	docLen   map[string]int
	avgDL    float64
	n        int
	built    bool
	version  int // bumped by the engine when the corpus changes
}

type posting struct {
	id string
	tf int
}

func newBM25Index() *bm25Index {
	return &bm25Index{df: map[string]int{}, postings: map[string][]posting{}, docLen: map[string]int{}}
}

// build indexes the given decisions. `what` and `why` are both indexed: the rationale is where the
// distinguishing identifiers usually live ("no MySQL dual-write for orders").
func (ix *bm25Index) build(decisions []Decision) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	ix.df = map[string]int{}
	ix.postings = map[string][]posting{}
	ix.docLen = map[string]int{}
	ix.n = 0

	total := 0

	for _, d := range decisions {
		if d.Quarantined || d.SupersededBy != "" {
			continue
		}

		counts := map[string]int{}
		for _, t := range lexTokens(d.What + " " + d.Why) {
			counts[t]++
		}

		if len(counts) == 0 {
			continue
		}

		length := 0

		for t, tf := range counts {
			ix.postings[t] = append(ix.postings[t], posting{id: d.ID, tf: tf})
			ix.df[t]++
			length += tf
		}

		ix.docLen[d.ID] = length
		total += length
		ix.n++
	}

	if ix.n > 0 {
		ix.avgDL = float64(total) / float64(ix.n)
	}

	ix.built = true
}

// score returns BM25 scores for every document matching at least one query term.
//
// k1 and b are the standard tuning constants and are config knobs rather than literals (Gate #0):
// k1 controls term-frequency saturation, b the document-length normalisation. The defaults are the
// values the field has converged on (Robertson & Zaragoza 2009 report k1 ∈ [1.2, 2.0], b ≈ 0.75).
func (ix *bm25Index) score(query string, cfg *Config, chain ...string) map[string]float64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	out := map[string]float64{}
	if !ix.built || ix.n == 0 {
		return out
	}

	k1 := cfg.F("rank.bm25_k1", 1.5, chain...)
	b := cfg.F("rank.bm25_b", 0.75, chain...)

	seen := map[string]bool{}

	for _, term := range lexTokens(query) {
		if seen[term] {
			continue // a repeated query term must not double-count its own idf
		}

		seen[term] = true

		df := ix.df[term]
		if df == 0 {
			continue
		}

		// Robertson/Sparck-Jones idf with the +0.5 smoothing, floored at 0 so a term appearing in
		// almost every document contributes nothing rather than a negative score.
		idf := math.Log(1 + (float64(ix.n)-float64(df)+0.5)/(float64(df)+0.5))

		for _, p := range ix.postings[term] {
			dl := float64(ix.docLen[p.id])
			tf := float64(p.tf)
			out[p.id] += idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*dl/ix.avgDL))
		}
	}

	return out
}

// lexTokens splits text into lowercase alphanumeric tokens, preserving `_` so identifiers such as
// `orders` and `order_events` survive as SINGLE tokens. That is deliberate and is
// most of the point: splitting them into "cost"/"records" would rebuild exactly the topical blur
// that made the dense retriever confuse the two subsystems in the first place.
func lexTokens(s string) []string {
	var (
		out []string
		cur strings.Builder
	)

	flush := func() {
		if cur.Len() == 0 {
			return
		}

		t := cur.String()
		cur.Reset()

		if len(t) > 1 && !lexStop[t] {
			out = append(out, t)
		}
	}

	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			cur.WriteRune(r)
		default:
			flush()
		}
	}

	flush()

	return out
}

// lexStop removes terms so common they carry no discriminative signal. Deliberately SHORT: negation
// words ("no", "not", "never", "without") are pointedly NOT stopped, because distinguishing them is
// half the reason this index exists.
var lexStop = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true, "were": true,
	"this": true, "that": true, "with": true, "from": true, "into": true, "than": true,
	"there": true, "their": true, "them": true, "then": true, "they": true,
	"have": true, "has": true, "had": true, "been": true, "being": true,
	"its": true, "our": true, "your": true, "you": true, "all": true, "any": true,
	"can": true, "will": true, "would": true, "should": false, // "should" is intent-bearing
}
