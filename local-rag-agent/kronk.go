// kronk.go wires Ardan Labs' Kronk (llama.cpp / GGUF inference, in-process via yzma — no daemon, no
// HTTP) into GoFr. The chat model is exposed as a GoFr custom ai.Model (registered with app.AddLLM),
// so ctx.LLM() drives fully-local inference while GoFr still supplies tracing, token metrics and a
// health check. The embedding model is a second in-process Kronk instance used for vector recall.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/datasource"
	"gofr.dev/pkg/gofr/logging"
)

// kronkLogger adapts GoFr's structured logger to Kronk's logger (a func(ctx, msg, args...)), so every
// line Kronk emits — backend install, model download/load, runtime notices — flows through GoFr's
// logging instead of raw stdout. Chatty download-progress lines are kept at debug; everything else is
// info. This is the same "make it a GoFr citizen" move as the custom ai.Model: one adapter, and a
// third-party library logs in your app's format and level.
func kronkLogger(lg logging.Logger) applog.Logger {
	return func(_ context.Context, msg string, args ...any) {
		msg = strings.TrimPrefix(strings.TrimSpace(msg), "\r")
		if msg == "" {
			return
		}

		if kv := kvPairs(args); kv != "" {
			msg += " " + kv
		}

		line := "kronk: " + msg
		if strings.Contains(msg, "Downloading") || strings.Contains(msg, "MB of") {
			lg.Debug(line) // per-chunk download progress — noisy, keep it at debug
			return
		}

		lg.Info(line)
	}
}

// kvPairs renders Kronk's structured args (alternating key, value) as "key=value" text.
func kvPairs(args []any) string {
	var b strings.Builder

	for i := 0; i+1 < len(args); i += 2 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}

		fmt.Fprintf(&b, "%v=%v", args[i], args[i+1])
	}

	if len(args)%2 == 1 {
		fmt.Fprintf(&b, " %v", args[len(args)-1])
	}

	return b.String()
}

// bootstrapKronk installs the llama.cpp backend (once, cached) and downloads + loads the chat and
// embedding GGUF models from their configured sources (Hugging Face refs or local paths). First run
// downloads a few hundred MB per model; later runs load from the local cache. It is generic: point
// CHAT_MODEL / EMBED_MODEL at any GGUF Kronk can resolve.
func bootstrapKronk(ctx context.Context, lg logging.Logger, chatSrc, embedSrc string, ctxWindow int) (chat, embed *kronk.Kronk, err error) {
	klog := kronkLogger(lg) // route all of Kronk's logging through GoFr

	lib, err := libs.New()
	if err != nil {
		return nil, nil, fmt.Errorf("kronk libs.New: %w", err)
	}

	if _, err = lib.Download(ctx, klog); err != nil {
		return nil, nil, fmt.Errorf("install llama.cpp backend: %w", err)
	}

	if err = kronk.Init(kronk.WithLibPath(lib.LibsPath())); err != nil {
		return nil, nil, fmt.Errorf("kronk.Init: %w", err)
	}

	mdls, err := models.New()
	if err != nil {
		return nil, nil, fmt.Errorf("kronk models.New: %w", err)
	}

	cp, err := mdls.Download(ctx, klog, chatSrc)
	if err != nil {
		return nil, nil, fmt.Errorf("download chat model %q: %w", chatSrc, err)
	}

	ep, err := mdls.Download(ctx, klog, embedSrc)
	if err != nil {
		return nil, nil, fmt.Errorf("download embed model %q: %w", embedSrc, err)
	}

	chatOpts := []model.Option{model.WithModelFiles(cp.ModelFiles), model.WithLog(klog)}
	if ctxWindow > 0 {
		chatOpts = append(chatOpts, model.WithContextWindow(ctxWindow))
	}

	if chat, err = kronk.New(chatOpts...); err != nil {
		return nil, nil, fmt.Errorf("load chat model: %w", err)
	}

	if embed, err = kronk.New(model.WithModelFiles(ep.ModelFiles), model.WithLog(klog)); err != nil {
		return nil, nil, fmt.Errorf("load embed model: %w", err)
	}

	return chat, embed, nil
}

// chatModel adapts a *kronk.Kronk to GoFr's ai.Model so it can be registered with app.AddLLM and
// used through ctx.LLM(). It also implements ai.Descriptor to report clean provider/model labels on
// metrics and traces ("kronk" / the model's id).
type chatModel struct {
	krn      *kronk.Kronk
	modelID  string
	maxTok   int
	thinking bool
	timeout  time.Duration
}

var (
	_ ai.Model      = (*chatModel)(nil)
	_ ai.Descriptor = (*chatModel)(nil)
)

// Name is GoFr's health key and default label for the model.
func (m *chatModel) Name() string { return "kronk" }

// ProviderName / ModelName give metrics and traces distinct provider and model labels.
func (m *chatModel) ProviderName() string { return "kronk" }
func (m *chatModel) ModelName() string    { return m.modelID }

// Chat runs a completion fully in-process on llama.cpp. GoFr wraps this call with a span and token
// metrics automatically; the mapping here just translates GoFr's message/option types to Kronk's
// request document and back. Kronk requires a context deadline, so one is added if the caller's
// context has none.
func (m *chatModel) Chat(ctx context.Context, messages []ai.Message, opts ...ai.Option) (*ai.Response, error) {
	var co ai.CallOptions
	for _, opt := range opts {
		opt(&co)
	}

	msgs := make([]model.D, 0, len(messages))
	for _, msg := range messages {
		msgs = append(msgs, model.D{"role": msg.Role, "content": msg.Content})
	}

	temperature := 0.2
	if co.Temperature != nil {
		temperature = *co.Temperature
	}

	maxTok := m.maxTok
	if co.MaxTokens != nil {
		maxTok = *co.MaxTokens
	}

	d := model.D{
		"messages":    msgs,
		"temperature": temperature,
		"max_tokens":  maxTok,
		// Ask thinking models (e.g. Qwen3) for a direct answer, not a <think> block, unless the
		// operator opts in via ENABLE_THINKING. Non-thinking models ignore this.
		"enable_thinking": m.thinking,
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}

	// Generate inside an explicit span and read the usage/content within it, so the actual token
	// generation (the bulk of a chat call) is attributed to the trace rather than left as an
	// unexplained gap. Kronk materializes the response lazily, so touching it here matters.
	genCtx, span := otel.Tracer("local-rag-agent").Start(ctx, "kronk.generate")

	resp, err := m.krn.Chat(genCtx, d)
	if err != nil {
		span.RecordError(err)
		span.End()

		return nil, err
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		span.End()
		return nil, fmt.Errorf("kronk: model returned no message")
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content) // force materialization in-span

	if resp.Usage != nil {
		span.SetAttributes(
			attribute.Int("gen.completion_tokens", resp.Usage.CompletionTokens),
			attribute.Float64("gen.tokens_per_sec", resp.Usage.TokensPerSecond),
		)
	}

	span.End()

	out := &ai.Response{
		Content: content,
		Model:   m.modelID,
	}

	if resp.Usage != nil {
		out.Usage = ai.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			ReasoningTokens:  resp.Usage.ReasoningTokens,
		}
	}

	return out, nil
}

// HealthCheck reports the local model as a GoFr datasource, so its readiness shows up on the health
// endpoint alongside SurrealDB.
func (m *chatModel) HealthCheck(_ context.Context) datasource.Health {
	return datasource.Health{
		Status: datasource.StatusUp,
		Details: map[string]any{
			"backend": "llama.cpp (kronk, in-process)",
			"model":   m.modelID,
		},
	}
}

// embedText turns text into a vector using the in-process embedding model. It opens a "kronk.embed"
// span so the local embedding step shows up in the request's trace — otherwise the biggest slice of
// an /ask (embedding the question) would be invisible, since embeddings don't go through ctx.LLM().
// Kronk requires a context deadline, so one is added if absent.
func embedText(ctx context.Context, krn *kronk.Kronk, timeout time.Duration, text string) ([]float32, error) {
	ctx, span := otel.Tracer("local-rag-agent").Start(ctx, "kronk.embed")
	defer span.End()

	span.SetAttributes(attribute.Int("embed.input_chars", len(text)))

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	resp, err := krn.Embeddings(ctx, model.D{"input": text})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		err = fmt.Errorf("kronk: no embedding returned")
		span.RecordError(err)

		return nil, err
	}

	span.SetAttributes(attribute.Int("embed.dimensions", len(resp.Data[0].Embedding)))

	return resp.Data[0].Embedding, nil
}
