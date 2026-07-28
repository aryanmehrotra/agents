// workflow-agent — a GoFr 1.58 service that turns a single high-level goal into a multi-step
// workflow across the whole fleet, without knowing which agents exist. It asks its LLM to break the
// goal into ordered, self-contained sub-tasks, then dispatches each one to the orchestrator's
// /assistant route — the same universal hub that routes any natural-language query to the right
// specialist — threading each step's output into the next. So "research GoFr's latest release and
// summarise it in three bullets" becomes research-agent → summarizer-agent, in one distributed trace.
//
// This is what makes composition generic: the workflow-agent hard-codes NO peer. It only knows the
// orchestrator (one config value), and the orchestrator already knows how to reach every specialist.
// Add or remove agents and existing workflows keep working. The model only *proposes* the plan; a
// deterministic Go guardrail *disposes* — the plan must be a JSON array of 1..maxSteps non-empty
// tasks, or it's refused before anything runs.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/service"
)

const (
	maxSteps        = 6    // a plan longer than this is refused — keeps a runaway goal bounded
	maxContextChars = 6000 // cap the accumulated cross-step context fed forward
	maxExcerpt      = 1200 // per-step output kept in the threaded context
)

func main() {
	app := gofr.New()

	// The ONLY peer this agent knows: the orchestrator hub. Every step is dispatched through it, so
	// the workflow-agent never names a specialist — routing (and thus which agents exist) stays the
	// orchestrator's concern. Resilient like every other inter-agent call.
	app.AddHTTPService("orchestrator", envOr("ORCHESTRATOR_URL", "http://localhost:8080"),
		&service.CircuitBreakerConfig{Threshold: 4, Interval: 2 * time.Second},
		&service.RateLimiterConfig{Requests: 20, Window: time.Second, Burst: 25},
	)

	app.POST("/workflow", runWorkflow)

	app.Run()
}

// stepResult records what one planned step did: the task, which specialist the hub routed it to, and
// the output (or the error, if that step failed — one bad step doesn't abort the whole workflow).
type stepResult struct {
	Step     int    `json:"step"`
	Task     string `json:"task"`
	RoutedTo string `json:"routed_to,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

func runWorkflow(c *gofr.Context) (any, error) {
	var in struct {
		Goal string `json:"goal"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		return map[string]any{"error": "provide a `goal` to plan a workflow for"}, nil
	}

	// 1. Plan: the model proposes an ordered list of sub-tasks.
	resp, err := c.LLM().Generate(c, planPrompt(goal), ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	steps, err := parsePlan(resp.Content)
	if err != nil {
		return map[string]any{
			"goal":    goal,
			"refused": true,
			"reason":  "could not plan the workflow: " + err.Error(),
		}, nil
	}

	// 2. Execute: dispatch each step to the hub, threading prior outputs forward as context.
	results := make([]stepResult, 0, len(steps))

	var ctxAcc strings.Builder

	for i, task := range steps {
		query := task
		if ctxAcc.Len() > 0 {
			query = task + "\n\nContext from earlier steps (use it if relevant):\n" + ctxAcc.String()
		}

		routedTo, output, derr := dispatch(c, query)

		sr := stepResult{Step: i + 1, Task: task, RoutedTo: routedTo, Output: output}
		if derr != nil {
			sr.Error = derr.Error()
			c.Logger.Errorf("workflow-agent: step %d (%q) failed: %v", i+1, task, derr)
		} else {
			appendContext(&ctxAcc, i+1, output)
		}

		results = append(results, sr)
	}

	// 3. Synthesize a final answer for the goal from everything the steps produced.
	result := synthesize(c, goal, results)

	return map[string]any{
		"goal":   goal,
		"steps":  results,
		"result": result,
	}, nil
}

// dispatch sends one sub-task to the orchestrator's /assistant hub (with the front-door API key) and
// returns which specialist it was routed to plus a readable output. It never names a specialist —
// the hub decides.
func dispatch(c *gofr.Context, query string) (routedTo, output string, err error) {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return "", "", err
	}

	headers := map[string]string{"X-Api-Key": envOr("API_KEY", "agents-demo-key")}

	resp, err := c.GetHTTPService("orchestrator").PostWithHeaders(c, "assistant", nil, body, headers)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	return parseAssistant(raw)
}

// parseAssistant pulls the routed-to service and a readable output out of the orchestrator's
// response envelope: {data:{route, routed_to, response:{data:{...specialist output...}}}}. It's
// tolerant — a specialist can return any shape, so it prefers common answer keys and falls back to
// compact JSON.
func parseAssistant(raw []byte) (routedTo, output string, err error) {
	var env struct {
		Data struct {
			RoutedTo string          `json:"routed_to"`
			Degraded bool            `json:"degraded"`
			Error    string          `json:"error"`
			Response json.RawMessage `json:"response"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return "", strings.TrimSpace(string(raw)), nil // not the expected shape; hand back the raw text
	}

	if env.Data.Degraded {
		return env.Data.RoutedTo, "", fmt.Errorf("%s", env.Data.Error)
	}

	// Unwrap GoFr's {data: ...} envelope around the specialist's own return, then read it.
	var inner struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(env.Data.Response, &inner)

	payload := inner.Data
	if len(payload) == 0 {
		payload = env.Data.Response
	}

	return env.Data.RoutedTo, readableOutput(payload), nil
}

// readableOutput turns a specialist's arbitrary JSON payload into a short human-readable string —
// preferring the common answer-bearing keys, else compact JSON.
func readableOutput(payload json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err == nil {
		for _, k := range []string{"answer", "tl_dr", "result", "summary", "redacted", "raw"} {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}

	return strings.TrimSpace(string(payload))
}

// synthesize asks the model to produce one final answer for the goal, grounded only in what the
// steps actually returned. Best-effort: if it fails, the per-step outputs are still returned.
func synthesize(c *gofr.Context, goal string, results []stepResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Output != "" {
			fmt.Fprintf(&b, "[step %d · %s] %s\n", r.Step, r.RoutedTo, r.Output)
		}
	}

	if b.Len() == 0 {
		return "No step produced usable output — see each step's error."
	}

	resp, err := c.LLM().Generate(c,
		"Using ONLY the step outputs below, give a single concise answer to the goal. Do not invent "+
			"anything not present in the outputs.\n\nGoal: "+goal+"\n\nStep outputs:\n"+b.String(),
		ai.WithTemperature(0.2))
	if err != nil {
		return strings.TrimSpace(b.String())
	}

	return strings.TrimSpace(resp.Content)
}

func planPrompt(goal string) string {
	return "You are a workflow planner for a team of specialist agents (they can: query business data; " +
		"triage support tickets; answer HR/IT questions from a knowledge base; review a code diff; " +
		"redact PII; summarize long text; answer questions over a SQL database; do web research from " +
		"https:// links with citations; extract structured fields from text; answer from local " +
		"documents; schedule a webhook reminder). Break the user's goal into an ORDERED list of simple, " +
		"self-contained sub-tasks, each solvable by ONE specialist. Prefer the fewest steps. Reply with " +
		"ONLY a JSON array of strings — no markdown, no explanation.\n\nGoal: " + goal
}

// parsePlan is the deterministic guardrail on the model's plan: strip any markdown fence, decode a
// strict JSON array of strings, and enforce 1..maxSteps non-empty tasks. A malformed or over-long
// plan is refused before any step runs — never silently truncated.
func parsePlan(raw string) ([]string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var steps []string
	if err := json.Unmarshal([]byte(s), &steps); err != nil {
		return nil, fmt.Errorf("plan was not a JSON array of task strings")
	}

	cleaned := make([]string, 0, len(steps))
	for _, t := range steps {
		if t = strings.TrimSpace(t); t != "" {
			cleaned = append(cleaned, t)
		}
	}

	if len(cleaned) == 0 {
		return nil, fmt.Errorf("plan had no usable steps")
	}

	if len(cleaned) > maxSteps {
		return nil, fmt.Errorf("plan has %d steps, more than the %d-step limit", len(cleaned), maxSteps)
	}

	return cleaned, nil
}

// appendContext threads a step's output into the running context for later steps, bounded so a long
// workflow can't blow up the prompt.
func appendContext(acc *strings.Builder, step int, output string) {
	if len(output) > maxExcerpt {
		output = output[:maxExcerpt] + "…"
	}

	if acc.Len()+len(output) > maxContextChars {
		return
	}

	fmt.Fprintf(acc, "[step %d] %s\n", step, output)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}
