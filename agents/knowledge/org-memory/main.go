package main

import (
	"os"
	"strings"

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
	q := splitCSV(ctx.Param("context"))
	q = append(q, splitCSV(ctx.Param("scope"))...)

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

	return map[string]any{
		"items": items,
		"count": len(items),
		"note":  "advisory prior decisions for this context; an empty result is valid and correct",
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
