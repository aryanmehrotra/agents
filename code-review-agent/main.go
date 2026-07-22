// code-review-agent — a GoFr 1.58 service that reviews a unified diff with an LLM
// and returns structured, file/line-anchored comments, the way a human reviewer
// would leave inline PR comments. Shows the same structured-JSON pattern as
// support-agent's triage, applied to a bigger, more structured payload, plus a
// streamed prose-review variant.
package main

import (
	"encoding/json"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/http/response"
)

func main() {
	app := gofr.New()

	app.POST("/review", review)              // structured inline comments (JSON)
	app.POST("/review/stream", reviewStream) // streamed prose review (SSE)

	app.Run()
}

type diffInput struct {
	Title string `json:"title"` // PR / commit title, optional context for the model
	Diff  string `json:"diff"`  // unified diff text
}

const reviewSystem = `You are a senior engineer reviewing a code diff. Respond with ONLY a JSON object:
{"summary":"one paragraph overall assessment","risk":"low|medium|high","comments":[{"file":"path","line":123,"severity":"nit|warning|blocker","comment":"specific, actionable feedback"}]}
Only comment on lines actually changed in the diff (+ lines). Focus on correctness, security and
maintainability over style. Use "line" as the line number in the NEW file version. Return
"comments":[] if the diff looks fine.`

func review(c *gofr.Context) (any, error) {
	var in diffInput
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: reviewSystem},
		{Role: ai.RoleUser, Content: "Title: " + in.Title + "\n\nDiff:\n" + in.Diff},
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

func reviewStream(c *gofr.Context) (any, error) {
	var in diffInput
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	stream, err := c.LLM().Stream(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a senior engineer. Write a concise prose code review " +
			"of the diff below: what's good, what's risky, and what to fix before merge. No preamble."},
		{Role: ai.RoleUser, Content: "Title: " + in.Title + "\n\nDiff:\n" + in.Diff},
	})
	if err != nil {
		return nil, err
	}

	return response.Stream{Source: stream}, nil
}
