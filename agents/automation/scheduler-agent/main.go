// scheduler-agent — a GoFr 1.58 service that turns a natural-language request ("in 10 minutes, POST
// a reminder to my webhook") into a scheduled task and fires it for real once it's due. Task
// planning/scheduling agents — tracking deadlines and firing reminders or follow-up actions without
// a human re-triggering them — are one of the clearest "always-on" agent use cases going into
// production now, alongside the broader shift from single-shot chat to agents that plan and execute
// multi-step work over time.
//
// The LLM only *proposes* a schedule (delay + a webhook URL + a short message) by turning free text
// into JSON. Because "firing" means making a real outbound HTTP request to a URL that came out of
// untrusted, model-parsed text, every URL is put through the same deterministic SSRF guardrail
// research-agent uses — BEFORE the task is even accepted, and again immediately before it fires: only
// http/https, no userinfo, no localhost/internal/metadata hosts, no literal loopback/private/
// link-local IPs. A request that resolves to a blocked target is refused at scheduling time, so it
// never sits in the queue waiting to fire.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/logging"
)

const (
	minDelaySeconds = 0
	maxDelaySeconds = 30 * 24 * 3600 // 30 days — a sane ceiling for a demo in-memory scheduler
	tickInterval    = time.Second
	fireTimeout     = 8 * time.Second
)

type taskStatus string

const (
	statusPending   taskStatus = "pending"
	statusFired     taskStatus = "fired"
	statusFailed    taskStatus = "failed"
	statusCancelled taskStatus = "cancelled"
)

// task is a single planned action: fire a POST to URL with Message once ScheduledAt is reached.
type task struct {
	ID          int64      `json:"id"`
	Request     string     `json:"request"`
	URL         string     `json:"url"`
	Message     string     `json:"message"`
	CreatedAt   time.Time  `json:"created_at"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	Status      taskStatus `json:"status"`
	Result      string     `json:"result,omitempty"`
}

// store is the in-memory task queue. A real deployment would back this with a datasource, but the
// scheduling/firing logic — and the guardrail in front of it — is identical either way.
type store struct {
	mu     sync.Mutex
	tasks  map[int64]*task
	nextID int64
}

var tasks = &store{tasks: make(map[int64]*task)}

func (s *store) create(request, taskURL, message string, delaySeconds int) *task {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := atomic.AddInt64(&s.nextID, 1)
	now := time.Now()

	t := &task{
		ID:          id,
		Request:     request,
		URL:         taskURL,
		Message:     message,
		CreatedAt:   now,
		ScheduledAt: now.Add(time.Duration(delaySeconds) * time.Second),
		Status:      statusPending,
	}
	s.tasks[id] = t

	return t
}

func (s *store) list() []*task {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}

	return out
}

func (s *store) get(id int64) (*task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]

	return t, ok
}

func (s *store) cancel(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok || t.Status != statusPending {
		return false
	}

	t.Status = statusCancelled

	return true
}

// due returns every pending task whose time has come, and atomically flips it to statusFired so the
// same task can never be picked up by two ticks.
func (s *store) due(now time.Time) []*task {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*task

	for _, t := range s.tasks {
		if t.Status == statusPending && !now.Before(t.ScheduledAt) {
			t.Status = statusFired
			out = append(out, t)
		}
	}

	return out
}

func (s *store) markResult(id int64, ok bool, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, found := s.tasks[id]
	if !found {
		return
	}

	if !ok {
		t.Status = statusFailed
	}

	t.Result = result
}

// fireClient makes the real outbound request when a task is due. It rides GoFr's OpenTelemetry
// transport (like research-agent) so a fired task shows up as a span, and re-checks the SSRF
// guardrail on every redirect hop — a URL that was safe at scheduling time can't be used to bounce a
// later redirect into an internal address.
var fireClient = &http.Client{
	Timeout:   fireTimeout,
	Transport: otelhttp.NewTransport(http.DefaultTransport),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}

		if ok, reason := isSafeURL(req.URL.String()); !ok {
			return fmt.Errorf("redirect blocked: %s", reason)
		}

		return nil
	},
}

func main() {
	app := gofr.New()

	go runScheduler(app.Logger())

	app.POST("/schedule", schedule)
	app.GET("/tasks", listTasks)
	app.GET("/tasks/{id}", getTask)
	app.DELETE("/tasks/{id}", cancelTask)

	app.Run()
}

// runScheduler is the background loop that actually fires due tasks — the reason this is an "agent
// that plans AND fires tasks" rather than just a reminder list. It runs for the lifetime of the
// process, independent of any single HTTP request.
func runScheduler(logger logging.Logger) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for now := range ticker.C {
		for _, t := range tasks.due(now) {
			go fireTask(logger, t)
		}
	}
}

// fireTask makes the real outbound POST. The guardrail already ran once at schedule time, but it
// runs again here (isSafeURL, plus the redirect check on fireClient) as defense in depth — a task
// can sit in the queue for a long time before it's due.
func fireTask(logger logging.Logger, t *task) {
	if ok, reason := isSafeURL(t.URL); !ok {
		logger.Errorf("scheduler-agent: task %d refused at fire time: %s", t.ID, reason)
		tasks.markResult(t.ID, false, "refused at fire time: "+reason)

		return
	}

	body, _ := json.Marshal(map[string]any{
		"id":       t.ID,
		"message":  t.Message,
		"fired_at": time.Now().UTC(),
	})

	req, err := http.NewRequest(http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		tasks.markResult(t.ID, false, err.Error())
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agents-scheduler-agent/1.0")

	resp, err := fireClient.Do(req)
	if err != nil {
		logger.Errorf("scheduler-agent: task %d failed to fire: %v", t.ID, err)
		tasks.markResult(t.ID, false, err.Error())

		return
	}
	defer resp.Body.Close()

	tasks.markResult(t.ID, true, fmt.Sprintf("HTTP %d", resp.StatusCode))
}

func schedule(c *gofr.Context) (any, error) {
	var in struct {
		Request string `json:"request"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Request) == "" {
		return map[string]any{"error": "request must not be empty"}, nil
	}

	resp, err := c.LLM().Generate(c, schedulePrompt(in.Request), ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	spec, err := parseScheduleResponse(resp.Content)
	if err != nil {
		return map[string]any{
			"request": in.Request,
			"refused": true,
			"reason":  "could not understand the schedule request: " + err.Error(),
		}, nil
	}

	if ok, reason := validateSpec(spec); !ok {
		return map[string]any{
			"request": in.Request,
			"parsed":  spec,
			"refused": true,
			"reason":  reason,
		}, nil
	}

	t := tasks.create(in.Request, spec.URL, spec.Message, spec.DelaySeconds)

	return map[string]any{"task": t}, nil
}

func listTasks(c *gofr.Context) (any, error) {
	return map[string]any{"tasks": tasks.list()}, nil
}

func getTask(c *gofr.Context) (any, error) {
	id, err := strconv.ParseInt(c.PathParam("id"), 10, 64)
	if err != nil {
		return map[string]any{"error": "invalid task id"}, nil
	}

	t, ok := tasks.get(id)
	if !ok {
		return map[string]any{"error": "task not found"}, nil
	}

	return t, nil
}

func cancelTask(c *gofr.Context) (any, error) {
	id, err := strconv.ParseInt(c.PathParam("id"), 10, 64)
	if err != nil {
		return map[string]any{"error": "invalid task id"}, nil
	}

	if !tasks.cancel(id) {
		return map[string]any{"error": "task not found or already fired"}, nil
	}

	return map[string]any{"id": id, "status": statusCancelled}, nil
}

// scheduleSpec is the shape the model is asked to produce — parsed and then validated
// deterministically before anything is scheduled or fired.
type scheduleSpec struct {
	DelaySeconds int    `json:"delay_seconds"`
	URL          string `json:"url"`
	Message      string `json:"message"`
}

func schedulePrompt(request string) string {
	return "You turn a natural-language scheduling request into JSON describing WHEN to act and WHAT " +
		"to POST to a webhook URL when the time comes. Reply with ONLY a single JSON object, no markdown " +
		"fences, no explanation, of the exact shape:\n" +
		`{"delay_seconds": <integer seconds from now>, "url": "<https:// webhook to notify>", ` +
		`"message": "<short text payload for the webhook>"}` + "\n" +
		"Convert relative times (\"in 10 minutes\", \"tomorrow at 9am\" — assume that's ~24h out if no " +
		"exact time is given) into delay_seconds. If the request gives no URL to notify, use " +
		`"https://example.com/webhook" as a placeholder.` + "\n\nRequest: " + request
}

// parseScheduleResponse is pure and deterministic: it strips markdown fences models sometimes add
// despite being told not to, then decodes strict JSON into a scheduleSpec. It never trusts the
// model's output beyond "this is well-formed JSON of the right shape" — validateSpec does the rest.
func parseScheduleResponse(raw string) (scheduleSpec, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var spec scheduleSpec
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return scheduleSpec{}, fmt.Errorf("not valid JSON: %w", err)
	}

	return spec, nil
}

// validateSpec is the guardrail: it runs BEFORE a task is ever accepted into the queue (and its URL
// check runs again at fire time). A delay out of bounds or a URL that resolves to an unsafe target is
// refused deterministically — never left to the model's restraint, and never silently clamped or
// "fixed" into something the caller didn't ask for.
func validateSpec(spec scheduleSpec) (bool, string) {
	if spec.DelaySeconds < minDelaySeconds {
		return false, "delay_seconds must not be negative"
	}

	if spec.DelaySeconds > maxDelaySeconds {
		return false, fmt.Sprintf("delay_seconds exceeds the %d-second (30-day) limit", maxDelaySeconds)
	}

	if ok, reason := isSafeURL(spec.URL); !ok {
		return false, "webhook url refused: " + reason
	}

	if strings.TrimSpace(spec.Message) == "" {
		return false, "message must not be empty"
	}

	return true, ""
}

// isSafeURL is the SSRF guardrail, identical in spirit to research-agent's: only http/https, no
// userinfo, no localhost/internal/metadata hostnames, no literal loopback/private/link-local IPs
// (which covers the classic 169.254.169.254 cloud-metadata target). A firing task makes a real
// outbound request, so this must run deterministically in Go — never left to the model's judgment.
func isSafeURL(raw string) (bool, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, "unparseable URL"
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return false, "scheme must be http or https, got " + u.Scheme
	}

	if u.User != nil {
		return false, "userinfo in URL is not allowed"
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false, "missing host"
	}

	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") ||
		strings.Contains(host, "metadata") {
		return false, "internal/local host is not allowed"
	}

	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return false, "literal IP is loopback/private/link-local and not allowed"
	}

	return true, ""
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
