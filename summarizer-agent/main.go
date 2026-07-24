// summarizer-agent — a GoFr 1.58 service that summarizes a long document or chat/email
// thread with an LLM: a structured breakdown (tl;dr, key points, action items, decisions,
// open questions) plus a streamed narrative summary. Long-document and thread summarization
// is one of the most common tightly-scoped sub-agent responsibilities teams wire behind a
// router — the same structured-JSON + streaming split as support-agent and pii-redaction-agent.
package main

import (
	"encoding/json"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/http/response"
)

func main() {
	app := gofr.New()

	app.POST("/summarize", summarize)              // structured breakdown (JSON)
	app.POST("/summarize/stream", summarizeStream) // streamed narrative summary (SSE)

	app.Run()
}

type document struct {
	Text string `json:"text"`
}

const summarizeSystem = `You summarize a long document, email thread or chat transcript. Respond
with ONLY a JSON object:
{"tl_dr":"one or two sentences","key_points":["..."],"action_items":["..."],"decisions":["..."],"open_questions":["..."]}
Use empty arrays for a category that doesn't apply. Keep every entry short and concrete.`

func summarize(c *gofr.Context) (any, error) {
	var d document
	if err := c.Bind(&d); err != nil {
		return nil, err
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: summarizeSystem},
		{Role: ai.RoleUser, Content: d.Text},
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

func summarizeStream(c *gofr.Context) (any, error) {
	var d document
	if err := c.Bind(&d); err != nil {
		return nil, err
	}

	stream, err := c.LLM().Stream(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a concise technical writer. Write a short narrative " +
			"summary of the document below, in prose, for someone who has no time to read the original. " +
			"No preamble."},
		{Role: ai.RoleUser, Content: d.Text},
	})
	if err != nil {
		return nil, err
	}

	return response.Stream{Source: stream}, nil
}
