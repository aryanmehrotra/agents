// pii-redaction-agent — a GoFr 1.58 service that detects PII in text with an LLM and returns
// a redacted version plus a structured entity list, the kind of compliance/redaction agent
// enterprises are wiring into support tickets, chat logs and document pipelines before the
// text hits storage, logs or a third-party model. Shows the same structured-JSON detection
// pattern as code-review-agent, but the LLM is limited to *detection* only — the actual text
// substitution happens deterministically in Go so a hallucinated span can't corrupt the output.
package main

import (
	"encoding/json"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/http/response"
)

func main() {
	app := gofr.New()

	app.POST("/redact", redact)              // structured redaction (JSON)
	app.POST("/redact/stream", redactStream) // streamed compliance rationale (SSE)

	app.Run()
}

type redactInput struct {
	Text string `json:"text"`
}

type entity struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type detection struct {
	Entities []entity `json:"entities"`
}

const detectSystem = `You detect personally identifiable information (PII) in text for a compliance
redaction pipeline. Respond with ONLY a JSON object:
{"entities":[{"type":"name|email|phone|ssn|credit_card|address|dob|ip_address|other","value":"exact substring as it appears in the text"}]}
Only include entities that literally appear in the text, copied verbatim so they can be located
and replaced. Return "entities":[] if nothing is found.`

func redact(c *gofr.Context) (any, error) {
	var in redactInput
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: detectSystem},
		{Role: ai.RoleUser, Content: in.Text},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	var d detection
	if jsonErr := json.Unmarshal([]byte(resp.Content), &d); jsonErr != nil {
		return map[string]any{"raw": resp.Content}, nil
	}

	redacted := in.Text
	risk := "low"

	for _, e := range d.Entities {
		if e.Value == "" {
			continue
		}

		redacted = strings.ReplaceAll(redacted, e.Value, "["+strings.ToUpper(e.Type)+"]")

		switch e.Type {
		case "ssn", "credit_card":
			risk = "high"
		default:
			if risk == "low" {
				risk = "medium"
			}
		}
	}

	return map[string]any{
		"redacted_text": redacted,
		"entities":      d.Entities,
		"risk":          risk,
	}, nil
}

func redactStream(c *gofr.Context) (any, error) {
	var in redactInput
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	stream, err := c.LLM().Stream(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a compliance analyst. In a short paragraph, explain " +
			"what categories of PII (if any) appear in the text below and why they matter for compliance. " +
			"No preamble, and do not repeat the raw PII values verbatim."},
		{Role: ai.RoleUser, Content: in.Text},
	})
	if err != nil {
		return nil, err
	}

	return response.Stream{Source: stream}, nil
}
