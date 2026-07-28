// support-agent — a GoFr 1.58 service that triages an incoming support ticket (or
// GitHub issue) with an LLM and streams a drafted reply over SSE. Shows the two
// everyday LLM primitives: a structured one-shot classification (ctx.LLM().Chat)
// and token streaming (ctx.LLM().Stream + response.Stream).
package main

import (
	"encoding/json"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/http/response"
)

func main() {
	app := gofr.New()

	app.POST("/triage", triage)              // structured classification (JSON)
	app.POST("/triage/stream", triageStream) // streamed drafted reply (SSE)

	app.Run()
}

type ticket struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

const triageSystem = `You triage software support tickets. Respond with ONLY a JSON object:
{"type":"bug|feature|question|spam","priority":"P0|P1|P2|P3","summary":"one line","needs_info":true|false,"suggested_reply":"a short reply to the user"}
P0 = outage/data-loss, P3 = trivial. Set needs_info=true if the ticket lacks a repro or version.`

func triage(c *gofr.Context) (any, error) {
	var t ticket
	if err := c.Bind(&t); err != nil {
		return nil, err
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: triageSystem},
		{Role: ai.RoleUser, Content: "Title: " + t.Title + "\n\nBody:\n" + t.Body},
	}, ai.WithTemperature(0.1))
	if err != nil {
		return nil, err
	}

	// The model returns JSON; pass it through untouched when valid, else wrap the raw text.
	if json.Valid([]byte(resp.Content)) {
		return json.RawMessage(resp.Content), nil
	}

	return map[string]any{"raw": resp.Content}, nil
}

func triageStream(c *gofr.Context) (any, error) {
	var t ticket
	if err := c.Bind(&t); err != nil {
		return nil, err
	}

	stream, err := c.LLM().Stream(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a friendly, concise support engineer. Draft a reply to the ticket below. No preamble."},
		{Role: ai.RoleUser, Content: "Title: " + t.Title + "\n\nBody:\n" + t.Body},
	})
	if err != nil {
		return nil, err
	}

	return response.Stream{Source: stream}, nil
}
