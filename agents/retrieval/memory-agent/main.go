// memory-agent — a conversational agent with real memory, on GoFr 1.58 + SurrealDB.
//
// SurrealDB is used as a SINGLE store for both layers of memory:
//   - working/session memory: recent turns of the conversation (records), and
//   - long-term semantic memory: the same turns, recalled by vector similarity.
//
// Text is embedded with ctx.LLM().(ai.Embedder) — the embeddings capability this repo adds to GoFr —
// stored alongside the content, and searched with SurrealDB's vector::similarity::cosine. When the
// window grows, old turns are compacted via the summarizer-agent.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/ai/llm"
	"gofr.dev/pkg/gofr/datasource/surrealdb"
	"gofr.dev/pkg/gofr/service"
)

// savedTotalTokens accumulates the estimated chat prompt tokens the bounded context has saved over
// re-sending the full history, across all turns since start-up.
var savedTotalTokens atomic.Int64

// sessionChars tracks the cumulative characters every session has ever exchanged (session →
// *atomic.Int64). It is the naive baseline for the token-savings metric — "what re-sending the whole
// conversation would cost" — kept independent of what's actually stored, so compaction pruning old
// turns doesn't understate the saving.
var sessionChars sync.Map

const (
	recentTurns = 6 // working-memory window fed back verbatim
	recallK     = 3 // semantically-relevant memories pulled from the long-term store

	// nomic-embed-text is an asymmetric model: it expects stored text and search text to be tagged
	// with different task prefixes. Tagging them separates the query and document spaces, which
	// sharply improves recall precision over embedding both sides identically.
	docPrefix   = "search_document: "
	queryPrefix = "search_query: "

	// recallFloor is the minimum cosine similarity for a fact to count as relevant. Tuned for
	// nomic-embed-text with the query/document prefixes above, whose related-pair scores cluster
	// around 0.5–0.7.
	recallFloor = 0.45

	// compactThreshold is the stored-turn count past which the oldest turns (everything beyond the
	// working window) are compacted into one summary via the summarizer-agent — a real inter-agent
	// call that keeps stored history bounded.
	compactThreshold = 12
)

func main() {
	app := gofr.New()

	// Two models: a chat model (default) and a dedicated embeddings model (named "embed"). Both
	// default to Ollama locally — memory needs a stateless chat model and a real embedding model, so
	// the claude-shim isn't used here — but either can be pointed at any OpenAI-compatible provider
	// (e.g. OpenAI, which serves both chat and embeddings) via CHAT_PROVIDER / EMBED_PROVIDER.
	app.AddLLM(&llm.Client{
		Provider: llm.Provider(envOr("CHAT_PROVIDER", string(llm.Ollama))),
		Model:    envOr("CHAT_MODEL", "llama3.2:1b"),
	})
	app.AddLLM(&llm.Client{
		Provider: llm.Provider(envOr("EMBED_PROVIDER", string(llm.Ollama))),
		Model:    envOr("EMBED_MODEL", "nomic-embed-text"),
	}, gofr.WithName("embed"))

	app.AddSurrealDB(surrealdb.New(&surrealdb.Config{
		Host:      envOr("SURREAL_HOST", "localhost"),
		Port:      envInt("SURREAL_PORT", 8010),
		Username:  envOr("SURREAL_USER", "root"),
		Password:  envOr("SURREAL_PASS", "root"),
		Namespace: envOr("SURREAL_NS", "agents"),
		Database:  envOr("SURREAL_DB", "memory"),
	}))

	// The one inter-agent dependency: when a session outgrows the window, memory-agent compacts the
	// oldest turns into a summary by calling the summarizer-agent over a resilient GoFr HTTP service
	// (circuit breaker + retry + health). Compaction is best-effort — if it's down, chat still works.
	app.AddHTTPService("summarizer-agent", envOr("SUMMARIZER_AGENT_URL", "http://localhost:8005"),
		&service.CircuitBreakerConfig{Threshold: 4, Interval: 2 * time.Second},
	)

	// Custom metrics: prove the memory-management payoff. Every turn we send a bounded prompt
	// (window + recalled facts) instead of the whole transcript; these track that gap.
	app.Metrics().NewGauge("app_memory_prompt_tokens",
		"Estimated chat prompt tokens this turn: bounded (window+recall) vs naive (full history).")
	app.Metrics().NewGauge("app_memory_context_tokens_saved",
		"Cumulative estimated chat prompt tokens saved by bounded context vs re-sending full history.")

	app.POST("/chat", chat)
	app.GET("/memory/{session}", listMemory)

	app.Run()
}

type chatIn struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

type memory struct {
	Content string  `json:"content"`
	Score   float64 `json:"score,omitempty"`
	Role    string  `json:"role,omitempty"`
	Ts      string  `json:"ts,omitempty"`
}

func chat(c *gofr.Context) (any, error) {
	var in chatIn
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	if in.SessionID == "" {
		in.SessionID = "default"
	}

	// Embed the incoming message as a search *query* for recall (asymmetric embedding — see embed).
	qvec, err := embed(c, queryPrefix+in.Message)
	if err != nil {
		return nil, err
	}

	recent := recentHistory(c, in.SessionID)          // working memory
	recalled := semanticRecall(c, in.SessionID, qvec) // long-term memory

	msgs := buildMessages(recalled, recent, in.Message)

	resp, err := c.LLM().Chat(c, msgs, ai.WithTemperature(0.3))
	if err != nil {
		return nil, err
	}

	// Measure the memory-management payoff: the bounded prompt we just sent vs naively re-sending the
	// entire conversation this turn. The difference is what the window + SurrealDB recall save.
	bounded, naive := contextTokens(in.SessionID, msgs, in.Message, resp.Content)
	saved := max(naive-bounded, 0)
	c.Metrics().SetGauge("app_memory_prompt_tokens", float64(bounded), "mode", "bounded")
	c.Metrics().SetGauge("app_memory_prompt_tokens", float64(naive), "mode", "naive")
	c.Metrics().SetGauge("app_memory_context_tokens_saved", float64(savedTotalTokens.Add(int64(saved))))

	// Working memory: store the raw turns verbatim (no embedding — they are replayed by position,
	// not searched).
	store(c, in.SessionID, "user", in.Message, nil)
	store(c, in.SessionID, "assistant", resp.Content, nil)

	// Long-term memory: index the user turn as a searchable "fact" only when it states something
	// durable, so questions and requests don't pollute recall. (A larger model could replace this
	// heuristic with LLM-based fact extraction — llama3.2:1b is not reliable enough for that.)
	if looksLikeFact(in.Message) {
		if fvec, ferr := embed(c, docPrefix+in.Message); ferr == nil {
			store(c, in.SessionID, "fact", in.Message, fvec)
		}
	}

	// Compaction is synchronous (it makes an inter-agent call that must share this request's live
	// context and trace); it only fires once a session outgrows the window, so most turns skip it.
	compactIfNeeded(c, in.SessionID)

	return map[string]any{
		"session_id": in.SessionID,
		"answer":     resp.Content,
		"recalled":   recalled, // surface what memory was used, so recall is observable
		"context_tokens": map[string]int{ // estimated; bounded is what we sent, naive is full history
			"bounded": bounded, "naive": naive, "saved": saved,
		},
	}, nil
}

// estTok is a rough, model-agnostic token estimate (~4 characters per token). It is used only to
// compare the bounded prompt against the naive full-history prompt on equal footing — not for
// billing, where the provider's real token counts (already recorded by GoFr) apply.
func estTok(s string) int { return (len(s) + 3) / 4 }

// contextTokens estimates the prompt tokens sent this turn (bounded: system + recalled facts +
// recent window + current message) against the naive alternative of re-sending the entire
// conversation every turn (system + full transcript + current message). The gap is what the sliding
// window and SurrealDB recall save on the chat model, and it grows with the conversation. The naive
// baseline is tracked in-memory (sessionChars) rather than from stored turns, so it keeps climbing
// even after compaction prunes old turns. The current turn is added to the baseline for next time.
func contextTokens(session string, sent []ai.Message, userMsg, answer string) (bounded, naive int) {
	for i := range sent {
		bounded += estTok(sent[i].Content)
	}

	acc, _ := sessionChars.LoadOrStore(session, new(atomic.Int64))
	prior := acc.(*atomic.Int64).Load() // every user+assistant char exchanged before this turn

	// Naive keeps the same system prompt (sent[0]) and current message (sent[len-1]) but re-sends the
	// whole transcript instead of the window.
	naive = estTok(sent[0].Content) + estTok(sent[len(sent)-1].Content) + int(prior)/4

	acc.(*atomic.Int64).Add(int64(len(userMsg) + len(answer)))

	return bounded, naive
}

// embed turns text into a vector via GoFr's optional Embedder capability, using the named "embed"
// model (a dedicated embeddings model, separate from the chat model).
func embed(c *gofr.Context, text string) ([]float32, error) {
	// Embed is a first-class LLM method: it returns ai.ErrEmbedNotSupported (surfaced here as-is) if
	// the "embed" model is chat-only, so a misconfiguration fails gracefully instead of panicking.
	resp, err := c.LLM("embed").Embed(c, []string{text})
	if err != nil {
		return nil, err
	}

	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return resp.Embeddings[0], nil
}

func store(c *gofr.Context, session, role, content string, vec []float32) {
	// CREATE ... CONTENT via Query: SurrealDB returns an array, which the typed Create() helper
	// cannot unmarshal into a single map.
	_, err := c.SurrealDB.Query(c, "CREATE memory CONTENT $d", map[string]any{
		"d": map[string]any{
			"session":   session,
			"role":      role,
			"content":   content,
			"embedding": vec,
			"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		c.Logger.Errorf("memory: store failed: %v", err)
	}
}

// recentHistory returns the last few turns in chronological order (working memory). SurrealDB's
// parser rejects ORDER BY on this field, so we sort the small result set in Go.
func recentHistory(c *gofr.Context, session string) []memory {
	rows, err := c.SurrealDB.Query(c,
		"SELECT role, content, ts FROM memory WHERE session = $s AND (role = 'user' OR role = 'assistant')",
		map[string]any{"s": session})
	if err != nil {
		c.Logger.Errorf("memory: recent query failed: %v", err)
		return nil
	}

	all := toMemories(rows)
	sort.Slice(all, func(i, j int) bool { return all[i].Ts < all[j].Ts })

	if len(all) > recentTurns {
		all = all[len(all)-recentTurns:]
	}

	return all
}

// semanticRecall returns the memories most similar to the query vector (long-term memory). The
// query vector is inlined as a literal array — passing it as a bound var yields a driver-serialized
// value that SurrealDB's vector function does not accept.
func semanticRecall(c *gofr.Context, session string, qvec []float32) []memory {
	// Cosine is scaled to an integer (math::round(sim * 10000)) because the SurrealDB driver decodes
	// a bare float score as 0; the integer survives and we scale it back in Go.
	query := fmt.Sprintf(
		"SELECT content, role, math::round(vector::similarity::cosine(embedding, %s) * 10000) AS score "+
			"FROM memory WHERE session = $s AND role = 'fact' "+
			"ORDER BY score DESC LIMIT %d",
		floatLiteral(qvec), recallK)

	rows, err := c.SurrealDB.Query(c, query, map[string]any{"s": session})
	if err != nil {
		c.Logger.Errorf("memory: recall query failed: %v", err)
		return nil
	}

	out := toMemories(rows)

	kept := out[:0]
	for i := range out {
		out[i].Score /= 10000 // undo the scaling
		if out[i].Score > recallFloor {
			kept = append(kept, out[i])
		}
	}

	return kept
}

func buildMessages(recalled, recent []memory, message string) []ai.Message {
	msgs := []ai.Message{{Role: ai.RoleSystem, Content: memorySystemPrompt(recalled)}}

	for _, t := range recent {
		role := ai.RoleUser
		if t.Role == "assistant" {
			role = ai.RoleAssistant
		}

		msgs = append(msgs, ai.Message{Role: role, Content: t.Content})
	}

	msgs = append(msgs, ai.Message{Role: ai.RoleUser, Content: message})

	return msgs
}

func memorySystemPrompt(recalled []memory) string {
	var b strings.Builder
	b.WriteString("You are a helpful assistant with long-term memory of this user.")

	if len(recalled) > 0 {
		b.WriteString(" Relevant things the user told you earlier:\n")
		for _, m := range recalled {
			b.WriteString("- " + m.Content + "\n")
		}
		b.WriteString("Use them naturally when they help; do not mention that you retrieved them.")
	}

	return b.String()
}

// compactIfNeeded summarizes old turns into a single memory once the session grows large, so the
// working window stays small without losing information. It reuses the summarizer-agent.
// compactIfNeeded keeps stored history bounded: once a session has more than compactThreshold raw
// turns, it summarizes everything older than the working window into a single recallable "summary"
// memory via the summarizer-agent (a resilient inter-agent HTTP call) and deletes those raw turns.
// It is best-effort — any failure is logged and the turn is unaffected.
func compactIfNeeded(c *gofr.Context, session string) {
	all := recentAll(c, session) // every user/assistant turn, oldest-first
	if len(all) <= compactThreshold {
		return
	}

	old := all[:len(all)-recentTurns] // keep the working window; compact the rest
	if len(old) < 2 {
		return
	}

	var b strings.Builder
	for _, m := range old {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}

	summary, err := summarize(c, b.String())
	if err != nil {
		c.Logger.Errorf("memory: compaction (summarizer-agent) failed: %v", err)
		return
	}

	// Store the summary as a recallable memory so the gist of pruned turns survives, then drop the
	// raw turns older than the newest kept turn (ts sorts lexically for RFC3339).
	if svec, verr := embed(c, docPrefix+summary); verr == nil {
		store(c, session, "summary", summary, svec)
	}

	cutoff := all[len(all)-recentTurns].Ts
	if _, derr := c.SurrealDB.Query(c,
		"DELETE memory WHERE session = $s AND (role = 'user' OR role = 'assistant') AND ts < $cutoff",
		map[string]any{"s": session, "cutoff": cutoff}); derr != nil {
		c.Logger.Errorf("memory: compaction prune failed: %v", derr)
	}

	c.Logger.Infof("memory: compacted %d turns into a summary for session %s", len(old), session)
}

// summarize asks the summarizer-agent to distil a transcript into one or two sentences, over the
// resilient HTTP service registered in main. It returns the tl_dr (or the raw text if the response
// isn't the structured shape).
func summarize(c *gofr.Context, transcript string) (string, error) {
	body, err := json.Marshal(map[string]string{"text": transcript})
	if err != nil {
		return "", err
	}

	resp, err := c.GetHTTPService("summarizer-agent").Post(c, "summarize", nil, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// summarizer-agent returns {tl_dr, key_points, ...} on success, or {raw: "..."} otherwise; both
	// may be wrapped in GoFr's {data: ...} envelope.
	var env struct {
		Data struct {
			TLDR string `json:"tl_dr"`
			Raw  string `json:"raw"`
		} `json:"data"`
	}

	if err := json.Unmarshal(data, &env); err != nil {
		return "", err
	}

	switch {
	case env.Data.TLDR != "":
		return env.Data.TLDR, nil
	case env.Data.Raw != "":
		return env.Data.Raw, nil
	default:
		return "conversation summary unavailable", nil
	}
}

// recentAll returns every user/assistant turn for a session, oldest-first (SurrealDB's parser
// rejects ORDER BY on ts, so the small set is sorted in Go).
func recentAll(c *gofr.Context, session string) []memory {
	rows, err := c.SurrealDB.Query(c,
		"SELECT role, content, ts FROM memory WHERE session = $s AND (role = 'user' OR role = 'assistant')",
		map[string]any{"s": session})
	if err != nil {
		return nil
	}

	all := toMemories(rows)
	sort.Slice(all, func(i, j int) bool { return all[i].Ts < all[j].Ts })

	return all
}

func listMemory(c *gofr.Context) (any, error) {
	session := c.PathParam("session")

	rows, err := c.SurrealDB.Query(c,
		"SELECT role, content, ts FROM memory WHERE session = $s",
		map[string]any{"s": session})
	if err != nil {
		return nil, err
	}

	return map[string]any{"session": session, "turns": rows}, nil
}

func toMemories(rows []any) []memory {
	out := make([]memory, 0, len(rows))

	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}

		mem := memory{}
		if v, ok := m["content"].(string); ok {
			mem.Content = v
		}
		if v, ok := m["role"].(string); ok {
			mem.Role = v
		}
		switch v := m["score"].(type) {
		case float64:
			mem.Score = v
		case float32:
			mem.Score = float64(v)
		case int64:
			mem.Score = float64(v)
		case int:
			mem.Score = float64(v)
		case uint64:
			mem.Score = float64(v)
		}
		if v, ok := m["ts"].(string); ok {
			mem.Ts = v
		}

		out = append(out, mem)
	}

	return out
}

// looksLikeFact is a cheap salience filter: a turn is worth remembering long-term when it states
// something durable about the user, not when it asks a question or issues a request. It keeps
// declarative statements ("My name is …", "I'm allergic to …") and drops interrogatives and
// commands ("recommend …", "what should I …", "… ?"). This avoids indexing the user's own questions
// as if they were facts, which otherwise dominates recall.
func looksLikeFact(msg string) bool {
	s := strings.ToLower(strings.TrimSpace(msg))
	if s == "" || strings.HasSuffix(s, "?") {
		return false
	}

	first := s
	if i := strings.IndexByte(s, ' '); i > 0 {
		first = s[:i]
	}

	switch first {
	case "what", "when", "where", "who", "whom", "why", "how", "which",
		"can", "could", "would", "should", "shall", "will",
		"do", "does", "did", "is", "are", "was", "were", "am",
		"recommend", "suggest", "tell", "give", "show", "list", "help", "find",
		"explain", "write", "create", "make", "describe", "name", "pick", "choose", "generate":
		return false
	}

	return true
}

// floatLiteral renders a vector as a SurrealQL array literal, e.g. [0.12,-0.03,…].
func floatLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', 6, 32)
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}

	return def
}
