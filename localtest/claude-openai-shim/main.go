// Command claude-openai-shim is a tiny OpenAI-compatible endpoint that proxies
// chat completions to the local `claude` CLI. It exists so the agents in this
// repo can be tested end-to-end with NO API key and NO model install — point an
// agent's LLM_BASE_URL at this server and it will answer via `claude -p`.
//
// It is a TEST HARNESS, not a production gateway: it shells out to the CLI, fakes
// token streaming by chunking the final text, and emulates tool-calling by asking
// the model for a strict JSON decision. Good enough to exercise GoFr's LLM + MCP
// plumbing locally; use a real provider (Groq/OpenAI/Ollama) for anything else.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const defaultAddr = ":8088"

// ---- OpenAI wire types (only the fields we use) ----

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Tools       []tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string, per OpenAI
}

type chatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Index        int         `json:"index"`
	Message      *message    `json:"message,omitempty"`
	Delta        *message    `json:"delta,omitempty"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---- claude CLI result shape ----

type claudeResult struct {
	Result string `json:"result"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func main() {
	addr := defaultAddr
	if v := os.Getenv("SHIM_ADDR"); v != "" {
		addr = v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "claude-shim", "object": "model", "owned_by": "claude-cli"},
		}})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	log.Printf("claude-openai-shim listening on %s (backend: claude CLI)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	prompt := buildPrompt(req.Messages, req.Tools)

	text, in, out, err := callClaude(r.Context(), prompt)
	if err != nil {
		http.Error(w, "claude cli: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Tool mode: parse the strict JSON decision and surface it as OpenAI tool_calls.
	if len(req.Tools) > 0 {
		if name, args, ok := parseToolDecision(text); ok {
			resp := baseResponse(req.Model, in, out)
			resp.Choices = []choice{{
				Index:        0,
				Message:      &message{Role: "assistant", ToolCalls: []toolCall{{ID: "call_" + name, Type: "function", Function: toolCallFunction{Name: name, Arguments: args}}}},
				FinishReason: "tool_calls",
			}}
			writeJSON(w, resp)
			return
		}
		// Not a tool call — fall through to a normal final answer (unwrap {"content":...}).
		text = unwrapFinal(text)
	}

	if req.Stream {
		streamText(w, req.Model, text, in, out)
		return
	}

	resp := baseResponse(req.Model, in, out)
	resp.Choices = []choice{{Index: 0, Message: &message{Role: "assistant", Content: text}, FinishReason: "stop"}}
	writeJSON(w, resp)
}

// buildPrompt flattens an OpenAI message array into a single prompt for `claude -p`,
// appending a strict-JSON tool protocol when tools are offered.
func buildPrompt(msgs []message, tools []tool) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			b.WriteString("[System instructions]\n" + m.Content + "\n\n")
		case "user":
			b.WriteString("[User]\n" + m.Content + "\n\n")
		case "assistant":
			if len(m.ToolCalls) > 0 {
				b.WriteString("[Assistant called tools]\n")
				for _, tc := range m.ToolCalls {
					b.WriteString(tc.Function.Name + "(" + tc.Function.Arguments + ")\n")
				}
				b.WriteString("\n")
			} else {
				b.WriteString("[Assistant]\n" + m.Content + "\n\n")
			}
		case "tool":
			b.WriteString("[Tool result]\n" + m.Content + "\n\n")
		}
	}

	if len(tools) > 0 {
		tj, _ := json.MarshalIndent(tools, "", "  ")
		b.WriteString("You can use these tools (JSON Schema):\n")
		b.Write(tj)
		b.WriteString("\n\nDecide the single next step. Reply with ONLY one JSON object, no prose and no code fences:\n")
		b.WriteString(`- to call a tool: {"action":"tool","name":"<tool_name>","arguments":{...}}` + "\n")
		b.WriteString(`- to answer the user: {"action":"final","content":"<your answer>"}` + "\n")
		b.WriteString("Prefer calling a tool when the answer depends on live data.\n")
	}

	return b.String()
}

func callClaude(ctx context.Context, prompt string) (text string, inTok, outTok int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--output-format", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err = cmd.Run(); err != nil {
		return "", 0, 0, fmt.Errorf("%v: %s", err, stderr.String())
	}

	var cr claudeResult
	if err = json.Unmarshal(stdout.Bytes(), &cr); err != nil {
		// Fall back to raw text if the CLI didn't return JSON.
		return strings.TrimSpace(stdout.String()), 0, 0, nil
	}
	return strings.TrimSpace(cr.Result), cr.Usage.InputTokens, cr.Usage.OutputTokens, nil
}

// parseToolDecision extracts {"action":"tool","name":...,"arguments":{...}} from the model text.
func parseToolDecision(text string) (name, argsJSON string, ok bool) {
	raw := extractJSON(text)
	if raw == "" {
		return "", "", false
	}
	var d struct {
		Action    string          `json:"action"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil || d.Action != "tool" || d.Name == "" {
		return "", "", false
	}
	args := "{}"
	if len(d.Arguments) > 0 {
		args = string(d.Arguments)
	}
	return d.Name, args, true
}

func unwrapFinal(text string) string {
	raw := extractJSON(text)
	if raw == "" {
		return text
	}
	var d struct {
		Action  string `json:"action"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err == nil && d.Content != "" {
		return d.Content
	}
	return text
}

// extractJSON returns the first balanced {...} object in s (tolerating code fences / prose).
func extractJSON(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "```json"))
	s = strings.TrimPrefix(s, "```")
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func streamText(w http.ResponseWriter, model, text string, in, out int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(c choice) {
		resp := baseResponse(model, in, out)
		resp.Object = "chat.completion.chunk"
		resp.Choices = []choice{c}
		b, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	send(choice{Index: 0, Delta: &message{Role: "assistant"}})
	for _, word := range chunkWords(text) {
		send(choice{Index: 0, Delta: &message{Content: word}})
		time.Sleep(15 * time.Millisecond)
	}
	send(choice{Index: 0, Delta: &message{}, FinishReason: "stop"})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// chunkWords splits text into word-ish chunks so the SSE demo visibly streams.
func chunkWords(s string) []string {
	parts := strings.SplitAfter(s, " ")
	if len(parts) == 0 {
		return []string{s}
	}
	return parts
}

func baseResponse(model string, in, out int) chatResponse {
	if model == "" {
		model = "claude-shim"
	}
	return chatResponse{
		ID:      "chatcmpl-shim",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Usage:   usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
