package main

import (
	"context"
	"os"
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

	seedConfig(cfg)

	// Recall is a GET so app.EnableMCP exposes it as the `recall_decisions` MCP tool — drop this one
	// service straight into Cursor / Claude Code. Writes (capture, feedback, config) are POST.
	app.EnableMCP()

	app.GET("/recall", recallHandler)
	app.GET("/decisions", decisionsHandler) // read-only: the whole memory, for the dashboard/brain view
	app.POST("/capture", captureHandler)
	app.POST("/feedback", feedbackHandler)
	app.GET("/config", getConfigHandler)
	app.POST("/config", setConfigHandler)

	// Scope hierarchy (parent → child), persisted; recall inherits along confirmed relations.
	app.GET("/hierarchy", hierarchyHandler)          // the tree + tags
	app.POST("/hierarchy", setParentHandler)         // set a parent (confirm a relation)
	app.POST("/hierarchy/propose", proposeHandler)   // generate proposals for the yes/no flow
	app.POST("/hierarchy/confirm", confirmRelationHandler) // yes/no on a proposed relation

	// Value metrics — the JSON dashboard + Prometheus gauges (scrape on METRICS_PORT → Grafana).
	app.GET("/stats", statsHandler)

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
		{"app_orgmem_tokens_injected_est", "org-memory: estimated tokens injected"},
		{"app_orgmem_net_tokens_saved_est", "org-memory: estimated NET tokens saved"},
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

	items, err := engine.Recall(ctx, q, chain...)
	if err != nil {
		return nil, err
	}

	publishMetrics(ctx)

	// Weak-match signal: if the best cosine only just clears the floor, the whole set is "reaching" —
	// loosely related, not a confident answer. Surface it so the caller can read the hedge.
	var topSim float64
	for _, it := range items {
		if it.Similarity > topSim {
			topSim = it.Similarity
		}
	}

	floor := cfg.F("retrieve.precision_floor", 0.30)
	weak := len(items) > 0 && topSim < floor+cfg.F("retrieve.weak_margin", 0.10)

	return map[string]any{
		"items":          items,
		"count":          len(items),
		"top_similarity": round(topSim),
		"weak":           weak,
		"note":           "advisory prior decisions for this context; an empty result is valid and correct",
	}, nil
}

// decisionsHandler (GET /decisions): read-only snapshot of the whole memory + a light summary, for the
// dashboard "brain" view. Embeddings are omitted (json:"-").
func decisionsHandler(ctx *gofr.Context) (any, error) {
	list := engine.List()

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
	}

	if err := ctx.Bind(&in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Key) == "" {
		return map[string]any{"error": "`key` is required"}, nil
	}

	cfg.Set(in.Key, in.Value, in.Scope)

	scope := in.Scope
	if scope == "" {
		scope = "org"
	}

	return map[string]any{"ok": true, "key": in.Key, "value": in.Value, "scope": scope}, nil
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
	m.SetGauge("app_orgmem_tokens_injected_est", float64(s.TokensInjected))
	m.SetGauge("app_orgmem_net_tokens_saved_est", float64(s.NetTokens))
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
