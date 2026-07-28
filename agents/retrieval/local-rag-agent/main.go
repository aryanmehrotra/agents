// local-rag-agent — a 100% local retrieval-augmented-generation agent on GoFr 1.58. Both the
// embedding model and the chat model run in-process on llama.cpp via Ardan Labs' Kronk (GGUF models,
// no daemon, no external API), and document vectors live in SurrealDB — used here as a vector
// database through GoFr's datasource. Ingested text is chunked, embedded and stored; a question is
// embedded, matched against stored chunks by cosine similarity, and answered by the local chat model
// grounded only in the retrieved passages, with [n] citations.
//
// The point: nothing leaves the machine — no prompt, no document, no embedding is sent to any hosted
// provider — yet GoFr still gives the local model tracing, token metrics and a health check (the
// chat model is registered as a custom ai.Model via app.AddLLM and used through ctx.LLM()).
//
// It's generic: point CHAT_MODEL / EMBED_MODEL at any GGUF Kronk can resolve, and SurrealDB at any
// instance. See the README for how to customise models, context window and recall.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/datasource/surrealdb"
)

// embedModel is the in-process embedding model, set once at start-up and used by the handlers. The
// chat model rides ctx.LLM(); the embedding model has no equivalent in GoFr 1.58's LLM interface, so
// it's held here and called directly through Kronk.
var (
	embedModel   *kronk.Kronk
	embedTimeout = 60 * time.Second
)

const (
	maxChunkChars = 800 // target size of a stored chunk; long paragraphs are split on word boundaries
	defaultK      = 4   // passages retrieved per question when the caller doesn't specify
)

func main() {
	app := gofr.New()

	// Bring up the local llama.cpp backend and load both GGUF models. First run downloads them
	// (a few hundred MB each); later runs load from cache. This blocks start-up on purpose — the
	// agent isn't ready until its models are.
	chatSrc := envOr("CHAT_MODEL", "unsloth/Qwen3-0.6B-Q8_0.gguf")
	embedSrc := envOr("EMBED_MODEL", "ggml-org/embeddinggemma-300m-qat-Q8_0.gguf")

	app.Logger().Infof("local-rag: loading local models (chat=%s embed=%s) — first run downloads them...",
		chatSrc, embedSrc)

	chatKrn, embedKrn, err := bootstrapKronk(context.Background(), app.Logger(), chatSrc, embedSrc, envInt("CHAT_CONTEXT", 0))
	if err != nil {
		app.Logger().Fatalf("local-rag: model bootstrap failed: %v", err)
	}

	embedModel = embedKrn
	embedTimeout = time.Duration(envInt("EMBED_TIMEOUT_SEC", 60)) * time.Second

	// Register the local chat model as a GoFr LLM. ctx.LLM() now runs llama.cpp in-process with
	// GoFr's tracing, token metrics and health wrapped around every call.
	app.AddLLM(&chatModel{
		krn:      chatKrn,
		modelID:  chatKrn.ModelInfo().ID,
		maxTok:   envInt("CHAT_MAX_TOKENS", 512),
		thinking: envBool("ENABLE_THINKING", false),
		timeout:  time.Duration(envInt("CHAT_TIMEOUT_SEC", 120)) * time.Second,
	})

	// SurrealDB is the vector database, wired through GoFr's datasource so c.SurrealDB is available
	// in handlers and its health is reported alongside the model. Provision the namespace/database
	// first (over SurrealDB's HTTP API) so they exist before the datasource issues its USE on
	// connect — a fresh SurrealDB then needs no manual setup.
	surrealHost := envOr("SURREAL_HOST", "localhost")
	surrealPort := envInt("SURREAL_PORT", 8000)
	surrealUser := envOr("SURREAL_USER", "root")
	surrealPass := envOr("SURREAL_PASS", "root")
	surrealNS := envOr("SURREAL_NS", "agents")
	surrealDB := envOr("SURREAL_DB", "rag")

	if err := provisionSurreal(context.Background(), surrealHost, surrealPort,
		surrealUser, surrealPass, surrealNS, surrealDB); err != nil {
		app.Logger().Errorf("local-rag: could not provision SurrealDB namespace/database "+
			"(is SurrealDB running on %s:%d?): %v", surrealHost, surrealPort, err)
	}

	app.AddSurrealDB(surrealdb.New(&surrealdb.Config{
		Host:      surrealHost,
		Port:      surrealPort,
		Username:  surrealUser,
		Password:  surrealPass,
		Namespace: surrealNS,
		Database:  surrealDB,
	}))

	app.POST("/ingest", ingest)
	app.POST("/ask", ask)

	app.Run()
}

// ingest chunks the given text, embeds each chunk with the local embedding model, and stores it in
// SurrealDB. One failed chunk is logged and skipped, not fatal.
func ingest(c *gofr.Context) (any, error) {
	var in struct {
		Text   string `json:"text"`
		Source string `json:"source"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	text := strings.TrimSpace(in.Text)
	if text == "" {
		return map[string]any{"error": "provide `text` to ingest (and optionally a `source` label)"}, nil
	}

	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = "untitled"
	}

	chunks := chunkText(text, maxChunkChars)

	stored := 0
	for _, ch := range chunks {
		vec, err := embedText(c, embedModel, embedTimeout, ch)
		if err != nil {
			c.Logger.Errorf("local-rag: embed failed for a chunk of %q: %v", source, err)
			continue
		}

		if err := storeChunk(c, source, ch, vec); err != nil {
			c.Logger.Errorf("local-rag: store failed for a chunk of %q: %v", source, err)
			continue
		}

		stored++
	}

	return map[string]any{
		"source":          source,
		"chunks_ingested": stored,
		"chunks_total":    len(chunks),
	}, nil
}

// ask embeds the question, retrieves the most similar stored chunks, and asks the local chat model
// to answer grounded only in them, citing passages as [n].
func ask(c *gofr.Context) (any, error) {
	var in struct {
		Question string `json:"question"`
		K        int    `json:"k"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	question := strings.TrimSpace(in.Question)
	if question == "" {
		return map[string]any{"error": "provide a `question`"}, nil
	}

	k := in.K
	if k <= 0 {
		k = defaultK
	}

	qvec, err := embedText(c, embedModel, embedTimeout, question)
	if err != nil {
		return nil, err
	}

	hits := searchChunks(c, qvec, k, recallFloor())
	if len(hits) == 0 {
		return map[string]any{
			"question": question,
			"answer":   "I have nothing ingested that's relevant to that — POST some text to /ingest first.",
			"sources":  []chunk{},
		}, nil
	}

	var ctxb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&ctxb, "[%d] (%s)\n%s\n\n", i+1, h.Source, h.Content)
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You answer using ONLY the numbered context passages provided. " +
			"Cite the passages you use inline as [n], matching their number. If the passages do not " +
			"contain the answer, say so plainly instead of guessing."},
		{Role: ai.RoleUser, Content: "Context:\n" + ctxb.String() + "\nQuestion: " + question},
	}, ai.WithTemperature(0.2))
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"question": question,
		"answer":   strings.TrimSpace(resp.Content),
		"sources":  hits, // the passages that grounded the answer, with similarity scores
	}, nil
}

// chunkText splits text into chunks no larger than maxChars, packing whole paragraphs together and
// splitting an over-long paragraph on word boundaries. It's deterministic and dependency-free so it
// can be unit-tested without a model.
func chunkText(text string, maxChars int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var (
		chunks []string
		cur    strings.Builder
	)

	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}

	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		if len(para) > maxChars {
			flush()
			chunks = append(chunks, splitLong(para, maxChars)...)

			continue
		}

		if cur.Len()+len(para)+2 > maxChars {
			flush()
		}

		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}

		cur.WriteString(para)
	}

	flush()

	return chunks
}

// splitLong breaks a single over-long paragraph into <=maxChars pieces, preferring the last word
// boundary so words aren't cut in half.
func splitLong(para string, maxChars int) []string {
	var out []string

	for len(para) > maxChars {
		cut := maxChars
		if i := strings.LastIndexByte(para[:maxChars], ' '); i > maxChars/2 {
			cut = i
		}

		out = append(out, strings.TrimSpace(para[:cut]))
		para = strings.TrimSpace(para[cut:])
	}

	if para != "" {
		out = append(out, para)
	}

	return out
}

// recallFloor is the minimum cosine similarity a chunk must have to be used. Default 0 keeps the
// top-k regardless of score; raise RECALL_FLOOR to drop weak matches.
func recallFloor() float64 {
	if v := os.Getenv("RECALL_FLOOR"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}

	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}

	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}

	return def
}
