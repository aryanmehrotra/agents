// research-agent — a GoFr 1.58 service that answers a question by fetching a set of
// caller-supplied source URLs and grounding an LLM's answer in the fetched content, with
// inline citations back to each source. Multi-source research-with-citations is one of the
// clearest "agent" patterns going into production right now — an assistant that reads real
// sources instead of answering from memory alone.
//
// Because the fetch targets are untrusted input (a caller, or a prompt-injected page a caller
// asked the model to "also check"), every URL is put through a deterministic SSRF guardrail
// before any outbound request is made: only http/https, no embedded credentials, no localhost,
// no cloud metadata hosts, and no address that resolves to a private/loopback/link-local range.
// A refused source is reported back with a reason — it is never fetched.
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/http/response"
)

const (
	maxSources    = 5
	maxBodyBytes  = 200_000
	snippetLength = 280
	fetchTimeout  = 8 * time.Second
)

// blockedHosts are well-known cloud metadata endpoints — reachable from inside a VPC/instance
// even though they're not "private" IPs in the RFC1918 sense, so the IP check alone misses them.
var blockedHosts = map[string]bool{
	"metadata.google.internal": true,
	"metadata.internal":        true,
	"metadata.azure.com":       true,
	"100.100.100.200":          true, // Alibaba Cloud metadata
}

func main() {
	app := gofr.New()

	app.POST("/research", research)              // structured answer + citations (JSON)
	app.POST("/research/stream", researchStream) // streamed narrative report (SSE)

	app.Run()
}

type researchInput struct {
	Question string   `json:"question"`
	Sources  []string `json:"sources"`
}

type citation struct {
	Index   int    `json:"index"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type refusal struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

type fetchedSource struct {
	url  string
	body string
}

func research(c *gofr.Context) (any, error) {
	var in researchInput
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	fetched, refused := gatherSources(in.Sources)

	citations := make([]citation, 0, len(fetched))
	for i, s := range fetched {
		citations = append(citations, citation{Index: i + 1, URL: s.url, Snippet: snippet(s.body)})
	}

	return map[string]any{
		"question":  in.Question,
		"answer":    synthesize(c, in.Question, fetched),
		"citations": citations,
		"refused":   refused,
	}, nil
}

func researchStream(c *gofr.Context) (any, error) {
	var in researchInput
	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	fetched, _ := gatherSources(in.Sources)

	stream, err := c.LLM().Stream(c, []ai.Message{
		{Role: ai.RoleSystem, Content: reportSystem},
		{Role: ai.RoleUser, Content: buildContext(in.Question, fetched)},
	})
	if err != nil {
		return nil, err
	}

	return response.Stream{Source: stream}, nil
}

const reportSystem = `You are a research assistant. Answer the user's question using ONLY the
numbered sources below — never from memory. Cite claims inline like [1], [2] matching the source
numbers. If the sources don't contain enough information to answer, say so plainly instead of
guessing. No preamble.`

// synthesize grounds the answer in whatever sources actually got fetched. It's best-effort: a
// failed LLM call still leaves the caller with the deterministic citations list to work with.
func synthesize(c *gofr.Context, question string, sources []fetchedSource) string {
	if len(sources) == 0 {
		return "No sources could be fetched; unable to produce a grounded answer."
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: reportSystem},
		{Role: ai.RoleUser, Content: buildContext(question, sources)},
	}, ai.WithTemperature(0.2))
	if err != nil {
		return "research synthesis unavailable: " + err.Error()
	}

	return strings.TrimSpace(resp.Content)
}

func buildContext(question string, sources []fetchedSource) string {
	var b strings.Builder

	b.WriteString("Question: " + question + "\n\n")

	for i, s := range sources {
		fmt.Fprintf(&b, "Source [%d] (%s):\n%s\n\n", i+1, s.url, truncate(s.body, 4000))
	}

	return b.String()
}

// gatherSources runs every candidate URL through the guardrail before fetching it — refused
// URLs are never touched by http.Client. Sources beyond maxSources are refused, not silently
// dropped, so the caller always sees why a source is missing from the citations.
func gatherSources(urls []string) ([]fetchedSource, []refusal) {
	var fetched []fetchedSource
	var refused []refusal

	client := &http.Client{Timeout: fetchTimeout}

	for i, raw := range urls {
		if i >= maxSources {
			refused = append(refused, refusal{URL: raw, Reason: fmt.Sprintf("exceeded max of %d sources per request", maxSources)})
			continue
		}

		if ok, reason := isSafeSource(raw); !ok {
			refused = append(refused, refusal{URL: raw, Reason: reason})
			continue
		}

		body, err := fetchURL(client, raw)
		if err != nil {
			refused = append(refused, refusal{URL: raw, Reason: err.Error()})
			continue
		}

		fetched = append(fetched, fetchedSource{url: raw, body: body})
	}

	return fetched, refused
}

// isSafeSource is the guardrail: a deterministic allowlist check run BEFORE any outbound
// request, so a hostile or prompt-injected URL (e.g. a cloud metadata endpoint or a local
// file:// path) is refused rather than executed. Never rely on the model to police its own
// tool input — this check runs regardless of what the model or caller intended.
func isSafeSource(raw string) (bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, "empty URL"
	}

	u, err := url.Parse(raw)
	if err != nil {
		return false, "not a valid URL"
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return false, fmt.Sprintf("scheme %q is not allowed — only http/https", u.Scheme)
	}

	if u.User != nil {
		return false, "URLs with embedded credentials are not allowed"
	}

	host := u.Hostname()
	if host == "" {
		return false, "URL has no host"
	}

	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return false, "localhost is not an allowed fetch target"
	}

	if blockedHosts[lower] {
		return false, "cloud metadata endpoints are not allowed fetch targets"
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return false, "could not resolve host"
	}

	for _, ip := range ips {
		if isDisallowedIP(ip) {
			return false, fmt.Sprintf("resolves to a non-public address (%s)", ip)
		}
	}

	return true, ""
}

// isDisallowedIP blocks loopback, RFC1918/private, link-local (incl. the 169.254.169.254
// cloud-metadata range), unspecified and multicast addresses — anything that isn't a plain
// public address.
func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func fetchURL(client *http.Client, raw string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", "research-agent/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", err
	}

	return stripHTML(string(body)), nil
}

// stripHTML is a minimal tag stripper — good enough to turn a fetched page into plain text for
// the LLM context without pulling in an HTML parsing dependency for a small agent.
func stripHTML(s string) string {
	var b strings.Builder

	inTag := false

	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}

	return strings.Join(strings.Fields(b.String()), " ")
}

func snippet(s string) string {
	return truncate(s, snippetLength)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}

	return strings.TrimSpace(s[:n]) + "…"
}
