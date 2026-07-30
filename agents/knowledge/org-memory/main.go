package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
)

// cfg and engine are process-wide: the engine is stateless glue over the Store/Embedder/Config
// interfaces, so a single instance serves all requests.
var (
	cfg    = NewConfig()
	engine *Engine
)

func main() {
	app := gofr.New()

	// Store and embedder are both behind interfaces. Store: in-memory | JSON file (ORG_MEMORY_PATH) |
	// embedded SQLite (ORG_MEMORY_DB) — zero infra either way. Embedder: REAL semantic embeddings via
	// GoFr's embed LLM by default (Ollama nomic-embed-text, or point EMBED_* at your orchestrator's
	// /v1/embeddings); explicit offline mode with ORGMEM_EMBED=local. No silent fallback — a down
	// provider fails loudly instead of corrupting the store with mismatched vectors.
	engine = NewEngine(mustStore(), chooseEmbedder(), cfg)

	// Optional cross-encoder re-ranking. Off unless a model is named, because it is a 635 MB GGUF and
	// most deployments should not pay for it. In-process via Kronk (llama.cpp/GGUF) — no daemon, no
	// container, no ONNX, and Metal-accelerated on Apple Silicon. See rerank.go for why the cheap
	// alternatives (BM25 hybrid, LLM-as-reranker) were tried and rejected on measurement.
	if m := strings.TrimSpace(os.Getenv("ORGMEM_RERANK_MODEL")); m != "" {
		engine = engine.WithReranker(newKronkReranker(m))
	}

	seedConfig(cfg)

	// Recall is a GET so app.EnableMCP exposes it as the `recall_decisions` MCP tool — drop this one
	// service straight into Cursor / Claude Code. Writes (capture, feedback, config) are POST.
	app.EnableMCP()

	// Admission gate FIRST: every mutating request is authenticated (token, or loopback-only when no
	// token is configured) before it reaches a handler. Closes the red-team's HIGH — unauthenticated
	// /capture writing attacker-controlled text straight into an agent's context.
	app.UseMiddleware(writeAuthMiddleware)

	app.GET("/recall", recallHandler)
	app.GET("/decisions", decisionsHandler) // read-only: the whole memory, for the dashboard/brain view
	app.POST("/capture", captureHandler)
	app.POST("/feedback", feedbackHandler)
	app.POST("/supersede", supersedeHandler) // retire an already-captured decision in favour of another
	app.GET("/config", getConfigHandler)
	app.POST("/config", setConfigHandler)
	app.GET("/config/history", configHistoryHandler) // Gate #0: every knob change is audited

	// Scope hierarchy (parent → child), persisted; recall inherits along confirmed relations.
	app.GET("/hierarchy", hierarchyHandler)                // the tree + tags
	app.POST("/hierarchy", setParentHandler)               // set a parent (confirm a relation)
	app.POST("/hierarchy/propose", proposeHandler)         // generate proposals for the yes/no flow
	app.POST("/hierarchy/confirm", confirmRelationHandler) // yes/no on a proposed relation

	// Value metrics — the JSON dashboard + Prometheus gauges (scrape on METRICS_PORT → Grafana).
	app.GET("/stats", statsHandler)

	// Gaps — the questions this memory repeatedly could not answer. An abstention is a correct output
	// here, which is exactly why gaps are otherwise invisible: "found nothing" looks the same whether
	// the corpus has a hole or the question was off-topic. This is the capture work list.
	app.GET("/gaps", gapsHandler)
	app.POST("/gaps/resolve", resolveGapHandler) // answer the near-miss question

	// Foundations — serves RESEARCH.md (the papers behind the design) to the console, so the "why"
	// is one concrete source of truth, not duplicated into the UI.
	app.GET("/research", researchHandler)

	// Auto-calibration — set the relevance floor from the corpus itself (no hand-picked number).
	app.POST("/calibrate", calibrateHandler)

	// Consolidation / forgetting — memory-health report: what's still retrievable vs decaying out.
	app.POST("/consolidate", consolidateHandler)

	// Calibrate once at startup so a fresh deployment gets a corpus-appropriate floor automatically,
	// unless a floor was explicitly seeded (ORGMEM_retrieve__precision_floor) or auto-calibration is
	// turned off (ORGMEM_AUTOCALIBRATE=off). Everything stays overridable live via POST /config.
	autoCalibrateOnStart(cfg)

	for _, g := range [][2]string{
		{"app_orgmem_decisions", "org-memory: decisions stored"},
		{"app_orgmem_recalls_total", "org-memory: recalls served"},
		{"app_orgmem_recall_empty_total", "org-memory: recalls returning nothing (correct restraint)"},
		{"app_orgmem_items_surfaced_total", "org-memory: decisions surfaced"},
		{"app_orgmem_feedback_helpful", "org-memory: helpful feedback"},
		{"app_orgmem_feedback_wrong", "org-memory: wrong feedback"},
	} {
		app.Metrics().NewGauge(g[0], g[1])
	}

	app.Run()
}

// mustStore picks the backing store: embedded SQLite (ORG_MEMORY_DB) → JSON file (ORG_MEMORY_PATH) →
// in-memory. All behind the same Store interface; all zero-infra.
func mustStore() Store {
	if db := strings.TrimSpace(os.Getenv("ORG_MEMORY_DB")); db != "" {
		if s, err := newSQLiteStore(db); err == nil {
			return s
		}
	}

	if p := strings.TrimSpace(os.Getenv("ORG_MEMORY_PATH")); p != "" {
		if fs, err := newFileStore(p); err == nil {
			return fs
		}
	}

	return newMemStore()
}

// chooseEmbedder wires embeddings. Explicit local mode (ORGMEM_EMBED=local) uses the deterministic,
// dependency-free embedder for offline/dev. Otherwise REAL semantic embeddings via any OpenAI-
// compatible /v1/embeddings endpoint — EMBED_BASE_URL defaults to a local Ollama (nomic-embed-text);
// point it at your orchestrator or a hosted provider, with EMBED_API_KEY if needed. No silent
// fallback: a down provider fails loudly rather than corrupting the store with mismatched vectors.
func chooseEmbedder() Embedder {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ORGMEM_EMBED")), "local") {
		return newFakeEmbedder()
	}

	return newHTTPEmbedder(
		envOr("EMBED_BASE_URL", "http://localhost:11434/v1"),
		envOr("EMBED_MODEL", "nomic-embed-text"),
		os.Getenv("EMBED_API_KEY"),
	)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return def
}

// recallHandler (GET → MCP tool `recall_decisions`): returns the few prior decisions that apply to a
// context — or nothing. Params: context=a,b,c (or scope=...), plus optional person/team for scoped config.
func recallHandler(ctx *gofr.Context) (any, error) {
	// Distinct input paths (so callers don't cram everything into one param):
	//   q       = the free-text question (what you're working on)      → embedded
	//   scope   = comma-separated scope tags (repo:x, service:y)        → matched, not embedded
	//   author  = a person facet (WHO), a shorthand for scope author:…  → matched
	//   context = legacy catch-all (still accepted; split into tag/text internally)
	q := splitCSV(ctx.Param("context"))
	q = append(q, splitCSV(ctx.Param("scope"))...)

	if question := strings.TrimSpace(ctx.Param("q")); question != "" {
		q = append(q, question)
	}

	if a := strings.TrimSpace(ctx.Param("author")); a != "" {
		q = append(q, "author:"+a)
	}

	var chain []string
	if p := strings.TrimSpace(ctx.Param("person")); p != "" {
		chain = append(chain, "person:"+p)
	}

	if tm := strings.TrimSpace(ctx.Param("team")); tm != "" {
		chain = append(chain, "team:"+tm)
	}

	items, diag, err := engine.RecallWithDiag(ctx, q, chain...)
	if err != nil {
		return nil, err
	}

	publishMetrics(ctx)

	// Confidence signals (Manmatha, Rath & Feng, SIGIR 2001 — score distributions): confidence is not an
	// absolute cosine but how much the top result STANDS OUT. Two DIFFERENT things were previously
	// OR-ed into one `weak` boolean, which made it unusable as an injection guard: it fired on 5/5
	// genuinely correct hits (up to similarity 0.83) purely because their runners-up were close, while
	// staying false on the confidently-wrong negation failures it most needed to catch. A tight cluster
	// of near-equally-relevant decisions is REDUNDANCY IN THE CORPUS, not low confidence in the answer,
	// so the two signals are now reported separately and neither is inferred from the other:
	//   weak      — the top match is barely above the bar at all (absolute confidence)
	//   ambiguous — the top match doesn't separate from its runner-up (flat distribution)
	// Both remain HEURISTICS standing in for a real query-performance predictor; the rigorous form is
	// a post-retrieval QPP gate (Clarity — Cronen-Townsend, Zhou & Croft, SIGIR 2002; WIG — Zhou &
	// Croft, SIGIR 2007), which RESEARCH.md §10c documents as the intended path. Splitting them makes
	// the signal measurable, not validated: that needs a labeled set of correct vs incorrect recalls.
	var top1, top2 float64

	for _, it := range items {
		switch {
		case it.Similarity > top1:
			top2 = top1
			top1 = it.Similarity
		case it.Similarity > top2:
			top2 = it.Similarity
		}
	}

	// `weak` is now a CALIBRATED statement, not a cosine margin. It fires when the best match is not
	// extreme enough to rule out noise, under the model fitted to this query's own score distribution
	// (scoredist.go). The knob is a probability an operator can argue about — "flag it if there is
	// more than a 1-in-1000 chance an irrelevant decision scores this high" — instead of the 0.10
	// cosine margin it replaces, which had no unit and did not transfer across corpora or models.
	// When no model can be fitted (sample too small, degenerate spread) NoiseP is 1 and we flag weak:
	// unknown confidence must read as low confidence, never as high.
	weakP := cfg.F("retrieve.weak_noise_p", 0.001)
	weak := len(items) > 0 && diag.NoiseP > weakP

	// `ambiguous` stays a distributional statement about the RESULT SET, not the answer: several
	// near-equally-relevant decisions means the corpus is redundant here, which is a different fact
	// from low confidence and must not be folded into it.
	ambiguous := len(items) >= 2 && (top1-top2) < cfg.F("retrieve.weak_min_margin", 0.03)

	// Feed the confidence loop, then let it decide whether to ask. A recall counts as confident when
	// it surfaced something and neither uncertainty signal fired; the rolling ratio of those is what
	// the ask-rate controls against (see ask.go).
	confident := len(items) > 0 && !weak && !ambiguous
	engine.health.record(confident)

	health, healthN := engine.RecallHealth()

	prompt := shouldAsk(askDecision{
		items: items, diag: diag, weak: weak, ambiguous: ambiguous,
		health: health, healthN: healthN, sampler: engine.sampler,
		// The free-text question only — NOT the scope tags. The gap log is keyed on the embedded text
		// (tags are matched, never embedded), so a prompt carrying "repo:x <question>" produces an
		// answer that can never match its own entry. Caught by rendering the hook and trying to
		// answer the question it printed.
		query:    strings.TrimSpace(strings.Join(freeText(q), " ")),
		silenced: engine.gaps.silenced(strings.Join(freeText(q), " ")),
	}, cfg, chain...)

	return map[string]any{
		"items": items,
		"count": len(items),
		// Best SURFACED similarity. On an abstention this is 0 because nothing surfaced — that 0 is an
		// absence, NOT a measured cosine, and no margin may be computed from it.
		"top_similarity": round(top1),
		// The honest abstention measurement: the best any decision in memory scored, and the bar it
		// failed to clear. "The closest thing scored 0.64 against a 0.65 floor" is calibration data;
		// "top_similarity 0.0" is what that same near-miss used to look like.
		"top_candidate_similarity": round(diag.TopSimilarity),
		"floor":                    round(diag.Floor),
		// P(an irrelevant decision scores this high) under this query's own fitted noise model — the
		// calibrated confidence number. `noise_fitted:false` means the sample was too small or too
		// degenerate to model, so no confidence claim is being made at all.
		"noise_p":      diag.NoiseP,
		"noise_fitted": diag.NoiseFitted,
		"weak":         weak,
		"ambiguous":    ambiguous,
		// Scan coverage. `budget_exceeded: true` means this result is PARTIAL — an empty result then
		// means "we ran out of time", not "nothing was relevant", and a caller must not read the
		// second from the first.
		"scanned":         diag.Scanned,
		"corpus_size":     diag.CorpusSize,
		"budget_exceeded": diag.BudgetExceeded,
		// The confidence loop: what fraction of recent recalls came back confident, and — when that
		// has slipped, or when THIS recall was uncertain — a question to close the gap.
		"confidence_ratio": round(health),
		"confidence_n":     healthN,
		"ask":              prompt,
		"note":             "advisory prior decisions for this context; an empty result is valid and correct",
	}, nil
}

// decisionsHandler (GET /decisions): read-only snapshot of the whole memory + a light summary, for the
// dashboard "brain" view. Embeddings are omitted (json:"-").
func decisionsHandler(ctx *gofr.Context) (any, error) {
	// Same isolation rule as /recall. A caller sees unlabelled decisions plus those matching the
	// org/tenant facets it asserts — never another tenant's, regardless of configuration.
	list := engine.List(append(splitCSV(ctx.Param("scope")), splitCSV(ctx.Param("context"))...)...)

	quarantined := 0
	for _, v := range list {
		if v.Decision.Quarantined {
			quarantined++
		}
	}

	return map[string]any{
		"count":       len(list),
		"quarantined": quarantined,
		"decisions":   list,
	}, nil
}

// captureHandler (POST /capture): record or reinforce a decision.
func captureHandler(ctx *gofr.Context) (any, error) {
	var in Decision
	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	d, err := engine.Capture(ctx, in)
	if err != nil {
		return map[string]any{"error": err.Error()}, nil
	}

	return map[string]any{"decision": d}, nil
}

// feedbackHandler (POST /feedback): the improvement loop. signal ∈ helpful|used|not_relevant|wrong|outdated.
func feedbackHandler(ctx *gofr.Context) (any, error) {
	var f Feedback
	if err := ctx.Bind(&f); err != nil {
		return nil, err
	}

	if err := engine.RecordFeedback(f); err != nil {
		return map[string]any{"error": err.Error()}, nil
	}

	publishMetrics(ctx)

	return map[string]any{"ok": true}, nil
}

// writeAuthMiddleware is the admission gate for every mutating request.
//
// It keys off the HTTP METHOD rather than a path list on purpose: a path allowlist silently fails
// open for any route added later, and "someone adds a write endpoint and forgets to guard it" is
// precisely how this hole would come back. Every non-idempotent method is guarded by construction,
// so a new POST route is protected before it is written.
//
// Reads stay open. Recall is the product, it is side-effect-free, and query text only ever becomes
// an embedding — the red-team confirmed query-side injection is structurally safe.
func writeAuthMiddleware(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if msg, allowed := writeGuard(cfg, r); !allowed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)

				_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})

				return
			}
		}

		inner.ServeHTTP(w, r)
	})
}

// supersedeHandler (POST /supersede): retire an already-captured decision in favour of an existing
// replacement — {"old":"d-…","new":"d-…"}. This is the reconciliation verb for conflicts that only
// become visible AFTER capture, when two independently-recorded decisions turn out to disagree. The
// retired row is kept in full and simply stops being surfaced, so the audit trail survives the fix.
func supersedeHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}

	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	if err := engine.Supersede(in.Old, in.New); err != nil {
		return map[string]any{"error": err.Error()}, nil
	}

	publishMetrics(ctx)

	return map[string]any{"ok": true, "superseded": in.Old, "by": in.New}, nil
}

// getConfigHandler (GET /config?key=...): read a resolved knob (Gate #0 — everything is tunable live).
func getConfigHandler(ctx *gofr.Context) (any, error) {
	key := strings.TrimSpace(ctx.Param("key"))
	if key == "" {
		return map[string]any{"error": "provide ?key="}, nil
	}

	return map[string]any{"key": key, "value": cfg.Str(key, "")}, nil
}

// setConfigHandler (POST /config): change a knob live — no code edit, no redeploy.
func setConfigHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		Scope string `json:"scope"`
		By    string `json:"by"` // who is making the change — recorded in the audit trail
	}

	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Key) == "" {
		return map[string]any{"error": "`key` is required"}, nil
	}

	if msg, ok := validateConfig(in.Key, in.Value); !ok {
		return map[string]any{"error": msg}, nil
	}

	cfg.SetBy(in.Key, in.Value, strings.TrimSpace(in.By), in.Scope)

	scope := in.Scope
	if scope == "" {
		scope = "org"
	}

	return map[string]any{"ok": true, "key": in.Key, "value": in.Value, "scope": scope}, nil
}

// configHistoryHandler (GET /config/history?limit=N): the append-only audit trail of knob changes.
// Config is behaviour in this system — a single write can silence recall entirely — so "who changed
// what, from what, when" is as much a part of the record as the decisions themselves.
func configHistoryHandler(ctx *gofr.Context) (any, error) {
	limit := 100
	if n, err := strconv.Atoi(strings.TrimSpace(ctx.Param("limit"))); err == nil && n > 0 {
		limit = n
	}

	changes := cfg.History(limit)

	return map[string]any{"changes": changes, "count": len(changes)}, nil
}

// statsHandler (GET /stats): the numerical value dashboard, and publishes the Prometheus gauges.
func statsHandler(ctx *gofr.Context) (any, error) {
	publishMetrics(ctx)

	return engine.ValueStats(), nil
}

// calibrateHandler (POST /calibrate): auto-set the relevance floor from the corpus's noise ceiling.
// Body (both optional): {"probes": ["off-topic sentence", ...], "margin": 0.03}. Empty → built-in
// generic probes. This is the "auto-adapt" path — no one hand-picks a cosine number.
func calibrateHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Probes []string `json:"probes"`
		Margin float64  `json:"margin"`
	}

	_ = ctx.Bind(&in)

	margin := in.Margin
	if margin <= 0 {
		margin = cfg.F("retrieve.calibration_margin", 0.03)
	}

	floor, ceiling, err := engine.Calibrate(ctx, in.Probes, margin)
	if err != nil {
		return map[string]any{"error": err.Error()}, nil
	}

	return map[string]any{
		"ok":              true,
		"precision_floor": floor,
		"noise_ceiling":   ceiling,
		"margin":          margin,
		"note":            "floor auto-set from the corpus; relevant queries score above the noise ceiling",
	}, nil
}

// autoCalibrateOnStart runs one calibration at boot so a fresh deployment gets a corpus-appropriate
// floor with zero tuning. Skipped if a floor was explicitly seeded (the operator's choice wins) or
// if ORGMEM_AUTOCALIBRATE=off. Non-fatal on error (e.g. embedder down): the absolute-floor default
// still applies and /calibrate can be retried live.
func autoCalibrateOnStart(c *Config) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ORGMEM_AUTOCALIBRATE")), "off") {
		return
	}

	if strings.TrimSpace(c.Str("retrieve.precision_floor", "")) != "" {
		return // an explicit seed (ORGMEM_retrieve__precision_floor) wins over auto-calibration
	}

	_, _, _ = engine.Calibrate(context.Background(), nil, c.F("retrieve.calibration_margin", 0.03))
}

// consolidateHandler (POST /consolidate): the memory-health report — how much of the store is still
// retrievable (hot) vs decayed below the forget floor (cold), and the coldest items with a reason.
// Read-only: it surfaces what runtime forgetting is already doing; it deletes nothing. Body optional:
// {"limit": 20} caps how many coldest items come back.
func consolidateHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Limit int `json:"limit"`
	}

	_ = ctx.Bind(&in)

	if in.Limit <= 0 {
		in.Limit = 20
	}

	if in.Limit > 200 { // bound attacker/fat-finger input
		in.Limit = 200
	}

	return engine.Consolidate(time.Now(), in.Limit), nil
}

// researchHandler (GET /research): returns the raw RESEARCH.md so the console can render the
// verified research foundations. The file is the single source of truth — the UI never duplicates it.
func researchHandler(ctx *gofr.Context) (any, error) {
	for _, p := range []string{"RESEARCH.md", "./RESEARCH.md"} {
		if b, err := os.ReadFile(p); err == nil {
			return map[string]any{"markdown": string(b)}, nil
		}
	}

	return map[string]any{"markdown": "# Research foundations\n\n_RESEARCH.md not found next to the binary._"}, nil
}

// publishMetrics mirrors the value stats into GoFr's Prometheus gauges (scraped on METRICS_PORT).
func publishMetrics(ctx *gofr.Context) {
	s := engine.ValueStats()
	m := ctx.Metrics()

	m.SetGauge("app_orgmem_decisions", float64(s.Decisions))
	m.SetGauge("app_orgmem_recalls_total", float64(s.Recalls))
	m.SetGauge("app_orgmem_recall_empty_total", float64(s.RecallsEmpty))
	m.SetGauge("app_orgmem_items_surfaced_total", float64(s.ItemsSurfaced))
	m.SetGauge("app_orgmem_feedback_helpful", float64(s.Helpful))
	m.SetGauge("app_orgmem_feedback_wrong", float64(s.Wrong))
}

// hierarchyHandler (GET /hierarchy): the persisted relations + scope tags in use, for the tree view.
func hierarchyHandler(ctx *gofr.Context) (any, error) { return engine.Hierarchy(), nil }

// setParentHandler (POST /hierarchy): set a confirmed parent→child relation (the user setting the tree).
func setParentHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Child  string `json:"child"`
		Parent string `json:"parent"`
	}

	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Child) == "" {
		return map[string]any{"error": "`child` is required"}, nil
	}

	engine.SetParent(in.Child, in.Parent)

	return map[string]any{"ok": true, "child": in.Child, "parent": in.Parent}, nil
}

// proposeHandler (POST /hierarchy/propose): generate parent proposals for tags without a relation.
func proposeHandler(ctx *gofr.Context) (any, error) {
	return map[string]any{"proposed": engine.Propose()}, nil
}

// confirmRelationHandler (POST /hierarchy/confirm): yes/no on a proposed relation.
func confirmRelationHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Child  string `json:"child"`
		Parent string `json:"parent"`
		Accept bool   `json:"accept"`
	}

	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	engine.ConfirmRelation(in.Child, in.Parent, in.Accept)

	return map[string]any{"ok": true}, nil
}

// validateConfig range-checks known numeric knobs so a hostile or fat-fingered write can't put the
// engine in a dangerous state (a negative precision_floor passes everything; a giant top_k was a
// crash/DoS and a store-dump vector). Defense at the write site complements the clamps at the read
// site in rank.go. Unknown keys are allowed (the config store is intentionally open-ended).
func validateConfig(key, value string) (string, bool) {
	bounds := map[string][2]float64{
		"retrieve.precision_floor":     {0, 1},
		"retrieve.weak_margin":         {0, 1},
		"retrieve.weak_min_margin":     {0, 1},
		"retrieve.top_k":               {1, 50},
		"retrieve.max_top_k":           {1, 200},
		"rank.band_width":              {0, 1},
		"rank.w_authority":             {0, 5},
		"rank.w_feedback":              {0, 5},
		"rank.recency_halflife_days":   {0.001, 100000},
		"rank.importance_saturation":   {1, 100},
		"retrieve.max_precision_floor": {0, 1},
		"hierarchy.max_inherit_depth":  {1, 1000},
		// prior_cap is the ceiling on how far ANY non-relevance signal can move a result. Bounding it
		// at the write site is what keeps "cosine decides" true: a large value re-opens the burial the
		// cap was introduced to close, letting a senior author's tangential note evict the answer.
		"rank.prior_cap":       {0, 1},
		"forget.floor":         {0, 1},
		"forget.halflife_days": {0, 100000},
		"retrieve.adaptive_z":  {0, 10},
		// Confidence knobs expressed as PROBABILITIES (scoredist.go) rather than cosine margins:
		// "at most this fraction of irrelevant decisions may clear the gate", and "flag weak above
		// this chance the top match is noise". Both are comparable across corpora and models.
		"retrieve.max_false_inject_rate": {0, 0.5},
		"retrieve.weak_noise_p":          {0, 1},
		"retrieve.adaptive_min_n":        {2, 100000},
		// 0 disables the scan budget (full scan). Bounded above so a fat-fingered value cannot turn
		// the "never blocks" guarantee back into an unbounded wait.
		"retrieve.budget_ms": {0, 60000},
		// The feedback ask-rate loop (ask.go). The rate is CONTROLLED against the confidence ratio,
		// not scheduled, so these bound the controller rather than setting a fixed cadence.
		"feedback.target_confidence_ratio": {0, 1},
		"feedback.sample_every_n":          {0, 10000},
		"feedback.health_window":           {1, 100000},
		"feedback.health_min_n":            {1, 100000},
		"feedback.ask_noise_p":             {0, 1},
		"feedback.ask_near_miss":           {0, 1},
		// Bounds for knobs the review found live-writable with NO validation at all. Each was a real
		// hole, not a hypothetical: a NaN authority weight propagated into Score and made /recall 500
		// (encoding/json refuses NaN); a NEGATIVE demote_per_notrel turns "not relevant" into a boost;
		// a negative rrf_k divides through zero into ±Inf fused scores. validateConfig's own doc says
		// it exists so a bad write cannot leave the engine in a dangerous state — it could only do
		// that for keys it knew about.
		"feedback.demote_per_notrel":   {0, 100},
		"feedback.boost_per_helpful":   {0, 100},
		"feedback.wrong_quarantine_at": {1, 100000},
		"rank.rrf_k":                   {1, 100000},
		"rank.rrf_w_relevance":         {0, 100},
		"rank.rrf_w_prior":             {0, 100},
		"rank.w_relevance":             {0, 100},
		"rank.w_recency":               {0, 100},
		"rank.w_importance":            {0, 100},
		"rank.w_retention":             {0, 100},
		"gaps.near_miss":               {0, 1},
		"gaps.min_content_tokens":      {0, 100},
		"gaps.max_tracked":             {1, 1000000},
		"forget.reinforce_per_use":     {0, 100},
		"forget.penalty_per_negative":  {0, 100},
		"rank.w_lexical":               {0, 10},
		"rank.bm25_k1":                 {0, 10},
		"rank.bm25_b":                  {0, 1},
	}

	key = strings.TrimSpace(key)

	b, known := bounds[key]

	// Open-ended families: `authority.<role>` is a weight on [0,1] and there is no fixed list of role
	// names, so an exact-key table can never cover it. Unbounded here meant `authority.sde2=NaN` was
	// accepted, flowed into the prior, and made every recall surfacing an authored decision return
	// 500 — the exact class of "dangerous state" this function exists to prevent.
	if !known && strings.HasPrefix(key, "authority.") && key != "authority.default_role" {
		b, known = [2]float64{0, 1}, true
	}

	if !known {
		return "", true
	}

	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return "`" + key + "` must be a number", false
	}

	// NaN/Inf slip past `f < lo || f > hi` (every comparison with NaN is false), which would leave a
	// knob in an undefined state — reject them explicitly before the range check.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "`" + key + "` must be a finite number", false
	}

	if f < b[0] || f > b[1] {
		return fmt.Sprintf("`%s` must be in [%g, %g]", key, b[0], b[1]), false
	}

	return "", true
}

// seedConfig loads any ORGMEM_<KEY>=value env vars as knobs (KEY with __ → .), so a deployment can set
// defaults without code. Everything still overridable live via POST /config.
func seedConfig(c *Config) {
	const prefix = "ORGMEM_"

	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			continue
		}

		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}

		name := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(parts[0], prefix), "__", "."))
		c.Set(name, parts[1])
	}
}

func splitCSV(s string) []string {
	var out []string

	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}

	return out
}

// gapsHandler (GET /gaps?min_count=2&limit=50): questions that repeatedly came CLOSE and found
// nothing — the corpus's own work list, ranked by how often they were asked.
//
// Three of this project's flagship "retrieval defects" turned out to be gaps, not ranking failures:
// the corpus held no fail-closed-for-Redis rule and no do-not-retry rule at all, so no reranker could
// have fixed them. That is the failure this endpoint makes visible before it gets misdiagnosed again.
func gapsHandler(ctx *gofr.Context) (any, error) {
	minCount := 2
	if n, err := strconv.Atoi(strings.TrimSpace(ctx.Param("min_count"))); err == nil && n > 0 {
		minCount = n
	}

	limit := 50
	if n, err := strconv.Atoi(strings.TrimSpace(ctx.Param("limit"))); err == nil && n > 0 {
		limit = n
	}

	gaps := engine.Gaps(minCount, limit)

	return map[string]any{
		"gaps":  gaps,
		"count": len(gaps),
		"note":  "questions asked repeatedly that nearly matched and returned nothing — candidates to capture",
	}, nil
}

// resolveGapHandler (POST /gaps/resolve): answer a near-miss question — {"query":"…","have_rule":bool}.
//
// The near-miss prompt used to offer two options and no way to submit either: /feedback requires a
// decision_id and an abstention has no decision. A question with no channel to receive its answer
// trains the reader to ignore the prompt, which costs more than never asking.
func resolveGapHandler(ctx *gofr.Context) (any, error) {
	var in struct {
		Query    string `json:"query"`
		HaveRule bool   `json:"have_rule"`
	}

	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Query) == "" {
		return map[string]any{"error": "`query` is required"}, nil
	}

	if !engine.ResolveGap(in.Query, in.HaveRule) {
		return map[string]any{"ok": false, "error": "that question is not on the gap list"}, nil
	}

	msg := "dropped — the silence was correct"
	if in.HaveRule {
		msg = "confirmed as a real gap; promoted to the top of the capture list"
	}

	return map[string]any{"ok": true, "query": in.Query, "result": msg}, nil
}

// freeText drops scope tags, leaving the question as the embedder and the gap log see it.
func freeText(parts []string) []string {
	out := make([]string, 0, len(parts))

	for _, p := range parts {
		if !isScopeTag(p) && strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}

	return out
}
