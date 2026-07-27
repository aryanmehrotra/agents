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
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"

	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/datasource"
)

// bootstrapKronk installs the llama.cpp backend (once, cached) and downloads + loads the chat and
// embedding GGUF models from their configured sources (Hugging Face refs or local paths). First run
// downloads a few hundred MB per model; later runs load from the local cache. It is generic: point
// CHAT_MODEL / EMBED_MODEL at any GGUF Kronk can resolve.
func bootstrapKronk(ctx context.Context, chatSrc, embedSrc string, ctxWindow int) (chat, embed *kronk.Kronk, err error) {
	lib, err := libs.New()
	if err != nil {
		return nil, nil, fmt.Errorf("kronk libs.New: %w", err)
	}

	if _, err = lib.Download(ctx, kronk.FmtLogger); err != nil {
		return nil, nil, fmt.Errorf("install llama.cpp backend: %w", err)
	}

	if err = kronk.Init(kronk.WithLibPath(lib.LibsPath())); err != nil {
		return nil, nil, fmt.Errorf("kronk.Init: %w", err)
	}

	mdls, err := models.New()
	if err != nil {
		return nil, nil, fmt.Errorf("kronk models.New: %w", err)
	}

	cp, err := mdls.Download(ctx, kronk.FmtLogger, chatSrc)
	if err != nil {
		return nil, nil, fmt.Errorf("download chat model %q: %w", chatSrc, err)
	}

	ep, err := mdls.Download(ctx, kronk.FmtLogger, embedSrc)
	if err != nil {
		return nil, nil, fmt.Errorf("download embed model %q: %w", embedSrc, err)
	}

	chatOpts := []model.Option{model.WithModelFiles(cp.ModelFiles)}
	if ctxWindow > 0 {
		chatOpts = append(chatOpts, model.WithContextWindow(ctxWindow))
	}

	if chat, err = kronk.New(chatOpts...); err != nil {
		return nil, nil, fmt.Errorf("load chat model: %w", err)
	}

	if embed, err = kronk.New(model.WithModelFiles(ep.ModelFiles)); err != nil {
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

	resp, err := m.krn.Chat(ctx, d)
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return nil, fmt.Errorf("kronk: model returned no message")
	}

	out := &ai.Response{
		Content: strings.TrimSpace(resp.Choices[0].Message.Content),
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

// embedText turns text into a vector using the in-process embedding model. Kronk requires a context
// deadline, so one is added if absent.
func embedText(ctx context.Context, krn *kronk.Kronk, timeout time.Duration, text string) ([]float32, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	resp, err := krn.Embeddings(ctx, model.D{"input": text})
	if err != nil {
		return nil, err
	}

	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("kronk: no embedding returned")
	}

	return resp.Data[0].Embedding, nil
}
