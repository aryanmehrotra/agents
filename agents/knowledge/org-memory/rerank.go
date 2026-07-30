package main

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"
)

// Cross-encoder re-ranking — the fix for negation blindness.
//
// WHY A CROSS-ENCODER AND NOT MORE RETRIEVAL. A bi-encoder compresses a whole decision into one
// vector before it has ever seen the query, so "fail open" and "fail closed" — one token apart in an
// otherwise identical sentence — land in nearly the same place. No amount of tuning the bi-encoder
// fixes that; the information was destroyed at encoding time. A cross-encoder reads the query and the
// document TOGETHER and scores the pair, which is the only architecture that can weigh a single
// contradicting token against everything the two texts share. Measured directly on this system's own
// failures (NevIR, Weller et al. EACL 2024, reports the same asymmetry across IR models: cross-
// encoders best, bi-encoders worst, on exactly this).
//
// The cheap approximation was tried first and failed. A BM25 lexical hybrid was built, swept, and
// rejected (`eval/validation/hybrid_lexical.md`): it made negation WORSE, because "add retries" and
// "do not retry" share three high-idf content terms and differ by low-idf negations, so bag-of-words
// pulls the polarities together harder than the embedding does. "Look at the actual tokens" is not
// what makes a cross-encoder work.
//
// WHY IN-PROCESS. The obvious deployment — a reranker container — does not exist for this machine:
// HuggingFace TEI publishes no arm64 image, and emulating amd64 is too slow to measure. Kronk runs
// llama.cpp/GGUF in-process via yzma with no daemon, no HTTP hop and no ONNX, which keeps the
// single-binary property the project is built around and gets Metal acceleration for free. It is
// already a dependency elsewhere in this repo, so it is a proven path rather than a new one.
//
// CASCADE, NOT REPLACEMENT. Re-ranking runs AFTER the relevance gate, over the survivors only
// (Wang/Lin/Metzler, SIGIR 2011). The gate, the fitted floor and the noise model all keep operating
// on cosine, so abstention behaviour is completely unchanged — the cross-encoder decides ORDER among
// things already judged relevant enough to surface, and can never resurrect something the gate
// rejected. That containment is deliberate: it is a 635 MB model whose failure mode should be a worse
// ordering, never a silent loss of the honesty guarantee.

// Reranker scores how well each document answers the query. Higher is better. An interface rather
// than a concrete type so ranking is testable without loading a model — the same dependency-injection
// discipline the Store and Embedder boundaries already follow.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
}

// kronkReranker lazily loads a GGUF cross-encoder the first time it is actually needed, so a
// deployment that never enables re-ranking never pays the load cost or the memory.
type kronkReranker struct {
	modelRef string

	once sync.Once
	krn  *kronk.Kronk
	err  error
}

func newKronkReranker(modelRef string) *kronkReranker {
	return &kronkReranker{modelRef: modelRef}
}

func (r *kronkReranker) load(ctx context.Context) error {
	r.once.Do(func() {
		quiet := applog.Logger(func(context.Context, string, ...any) {})

		lib, err := libs.New()
		if err != nil {
			r.err = err
			return
		}

		if _, r.err = lib.Download(ctx, quiet); r.err != nil {
			return
		}

		if r.err = kronk.Init(kronk.WithLibPath(lib.LibsPath())); r.err != nil {
			return
		}

		mdls, err := models.New()
		if err != nil {
			r.err = err
			return
		}

		mp, err := mdls.Download(ctx, quiet, r.modelRef)
		if err != nil {
			r.err = err
			return
		}

		r.krn, r.err = kronk.New(model.WithModelFiles(mp.ModelFiles), model.WithLog(quiet))
	})

	return r.err
}

func (r *kronkReranker) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if err := r.load(ctx); err != nil {
		return nil, err
	}

	resp, err := r.krn.Rerank(ctx, model.D{"query": query, "documents": docs})
	if err != nil {
		return nil, err
	}

	out := make([]float64, len(docs))
	for _, d := range resp.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = float64(d.RelevanceScore)
		}
	}

	return out, nil
}

// rerankCandidates runs the expensive stage over a WIDE, UNGATED pool and lets it decide both order
// and relevance.
//
// This inverts the obvious wiring, and the measurement is why. Gating on cosine first and re-ranking
// the survivors sounds safe, but it cannot work: for "should we fail closed when redis is down" only
// THREE decisions cleared the 0.653 cosine floor, and not one of them was a fail-closed rule — the
// corpus holds eleven. The bi-encoder had already discarded every correct answer before the
// cross-encoder was consulted, so re-ranking could only reshuffle three wrong ones. **A cascade
// cannot rescue what its first stage threw away**, and a floor tuned for a bi-encoder is exactly the
// stage that throws away polarity mismatches, because to a bi-encoder "fail open" IS the best match
// for "fail closed".
//
// So when re-ranking is enabled the cosine stage becomes pure RECALL — take the top `rerank_recall_n`
// by similarity with no floor at all — and the relevance decision moves onto the cross-encoder score,
// where polarity is actually visible. This is what cascade ranking means: each stage both filters and
// orders, and the cheap stage is tuned for recall while the expensive one supplies precision
// (Wang/Lin/Metzler, SIGIR 2011).
//
// Abstention is preserved, not weakened — it just runs on a better signal. `rerank_floor` is the
// minimum cross-encoder score worth surfacing, and an empty result remains a correct answer.
func rerankCandidates(ctx context.Context, rr Reranker, query string, cands []scored, cfg *Config, chain ...string) ([]RecalledItem, bool) {
	if rr == nil || len(cands) == 0 {
		return nil, false
	}

	recallN := cfg.I("rank.rerank_recall_n", 50, chain...)
	if recallN < 1 {
		return nil, false
	}

	pool := make([]scored, 0, len(cands))

	for _, c := range cands {
		if c.d.Quarantined || c.d.SupersededBy != "" {
			continue
		}

		pool = append(pool, c)
	}

	if len(pool) == 0 {
		return nil, false
	}

	sort.SliceStable(pool, func(i, j int) bool { return pool[i].sim > pool[j].sim })

	if len(pool) > recallN {
		pool = pool[:recallN]
	}

	docs := make([]string, len(pool))
	for i, c := range pool {
		docs[i] = c.d.What
		if c.d.Why != "" {
			docs[i] += " — " + c.d.Why
		}
	}

	budget := time.Duration(cfg.F("rank.rerank_budget_ms", 5000, chain...) * float64(time.Millisecond))

	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	scores, err := rr.Rerank(rctx, query, docs)
	if err != nil || len(scores) != len(pool) {
		return nil, false // degrade to the dense path; never fail the recall
	}

	order := make([]int, len(pool))
	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })

	floor := cfg.F("rank.rerank_floor", 0.05, chain...)
	topK := cfg.I("retrieve.top_k", 3, chain...)

	out := make([]RecalledItem, 0, topK)

	for _, idx := range order {
		if scores[idx] < floor {
			break // ordered, so everything after is also below the bar
		}

		c := pool[idx]

		out = append(out, RecalledItem{
			Decision: c.d, Similarity: round(c.sim), Score: round(scores[idx]),
			Guidance: render(c.d, cfg, chain...),
		})

		if len(out) >= topK {
			break
		}
	}

	return out, true
}

// rerankItems reorders already-gated results by cross-encoder score.
//
// Only the top `rerank_top_n` are scored, because a cross-encoder is O(candidates) model calls and
// the whole point of the cascade is that the expensive stage sees few things. Scores are mapped back
// onto the ORIGINAL similarity values in rank order, exactly as the lexical hybrid does, so the rest
// of the system keeps reading one relevance scale and `similarity` stays comparable across requests.
//
// A failure here — model missing, load error, timeout — returns the input untouched. Ranking must
// degrade to plain dense retrieval rather than fail the recall: this is an optional accuracy stage
// sitting in an agent's prompt path, and an advisory system that returns nothing because its reranker
// is unavailable is strictly worse than one that returns its second-best ordering.
func rerankItems(ctx context.Context, rr Reranker, query string, items []RecalledItem, cfg *Config, chain ...string) []RecalledItem {
	if rr == nil || len(items) < 2 {
		return items
	}

	topN := cfg.I("rank.rerank_top_n", 20, chain...)
	if topN < 2 {
		return items
	}

	n := min(topN, len(items))

	head := items[:n]

	docs := make([]string, n)
	for i, it := range head {
		docs[i] = it.Decision.What
		if it.Decision.Why != "" {
			docs[i] += " — " + it.Decision.Why
		}
	}

	budget := time.Duration(cfg.F("rank.rerank_budget_ms", 2000, chain...) * float64(time.Millisecond))

	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	scores, err := rr.Rerank(rctx, query, docs)
	if err != nil || len(scores) != n {
		return items // degrade to dense-only ordering
	}

	sims := make([]float64, n)
	for i, it := range head {
		sims[i] = it.Similarity
	}

	sort.Sort(sort.Reverse(sort.Float64Slice(sims)))

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })

	out := make([]RecalledItem, 0, len(items))

	for rank, idx := range order {
		it := head[idx]
		it.Similarity = round(sims[rank])
		it.Score = it.Similarity

		out = append(out, it)
	}

	return append(out, items[n:]...)
}
