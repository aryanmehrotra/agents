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

	// Zero-config local by default: an embedded, dependency-free store + a local deterministic
	// embedder, so it runs offline with no API keys. Both are behind interfaces — set ORG_MEMORY_PATH
	// for on-disk persistence, and swap in an LLM-backed embedder / sqlite-vec / Postgres anytime.
	engine = NewEngine(mustStore(), newFakeEmbedder(), cfg)

	seedConfig(cfg)

	// Recall is a GET so app.EnableMCP exposes it as the `recall_decisions` MCP tool — drop this one
	// service straight into Cursor / Claude Code. Writes (capture, feedback, config) are POST.
	app.EnableMCP()

	app.GET("/recall", recallHandler)
	app.POST("/capture", captureHandler)
	app.POST("/feedback", feedbackHandler)
	app.GET("/config", getConfigHandler)
	app.POST("/config", setConfigHandler)

	app.Run()
}

// mustStore picks on-disk persistence when ORG_MEMORY_PATH is set, else in-memory.
func mustStore() Store {
	if p := strings.TrimSpace(os.Getenv("ORG_MEMORY_PATH")); p != "" {
		if fs, err := newFileStore(p); err == nil {
			return fs
		}
	}

	return newMemStore()
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
