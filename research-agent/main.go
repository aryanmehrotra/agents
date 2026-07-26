// research-agent — a GoFr 1.58 service for multi-source web research with citations. Give it a
// question that includes one or more https:// links, and it fetches every source it's given, reads
// the real page content, and asks the LLM to answer grounded ONLY in those sources — with inline
// [n] citations back to the numbered source list. "Research assistant" agents that browse multiple
// sources and answer with citations (Perplexity-style, ChatGPT Deep Research) are one of the most
// visible AI-agent categories going into production in 2026.
//
// Because the URLs it fetches come straight from user-supplied, untrusted text, every URL goes
// through a deterministic guardrail BEFORE any outbound request is made: only http/https, no
// userinfo, no localhost/internal/metadata hosts, and no literal loopback/private/link-local IPs
// (the classic SSRF/cloud-metadata targets). The same check runs again on every redirect hop, so a
// safe URL can't be used to bounce the fetch into an internal address afterwards.
package main

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxSources      = 5
	maxFetchBytes   = 300 * 1024
	maxExcerptChars = 4000
	fetchTimeout    = 8 * time.Second
)

var httpClient = &http.Client{
	Timeout: fetchTimeout,
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

	app.POST("/research", research)

	app.Run()
}

type sourceResult struct {
	N       int    `json:"n"`
	URL     string `json:"url"`
	Fetched bool   `json:"fetched"`
	Refused bool   `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

func research(c *gofr.Context) (any, error) {
	var in struct {
		Question string `json:"question"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	urls := extractURLs(in.Question)
	if len(urls) == 0 {
		return map[string]any{
			"question": in.Question,
			"sources":  []sourceResult{},
			"answer": "I only research from sources you give me — include one or more https:// " +
				"links in your question and I'll fetch, read and cite them.",
		}, nil
	}

	if len(urls) > maxSources {
		urls = urls[:maxSources]
	}

	sources := make([]sourceResult, 0, len(urls))
	for i, u := range urls {
		sources = append(sources, fetchSource(c, i+1, u))
	}

	return map[string]any{
		"question": in.Question,
		"sources":  sources,
		"answer":   synthesize(c, in.Question, sources),
	}, nil
}

// fetchSource runs the guardrail before making any request, then fetches and extracts readable
// text. It never returns an error: a refused or failed source is just reported as such, so one bad
// URL in a list doesn't fail the whole request.
func fetchSource(c *gofr.Context, n int, rawURL string) sourceResult {
	if ok, reason := isSafeURL(rawURL); !ok {
		return sourceResult{N: n, URL: rawURL, Refused: true, Reason: reason}
	}

	req, err := http.NewRequestWithContext(c, http.MethodGet, rawURL, nil)
	if err != nil {
		return sourceResult{N: n, URL: rawURL, Reason: err.Error()}
	}

	req.Header.Set("User-Agent", "agents-research-agent/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return sourceResult{N: n, URL: rawURL, Reason: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return sourceResult{N: n, URL: rawURL, Reason: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return sourceResult{N: n, URL: rawURL, Reason: err.Error()}
	}

	text := stripHTML(string(body))
	if len(text) > maxExcerptChars {
		text = text[:maxExcerptChars]
	}

	return sourceResult{N: n, URL: rawURL, Fetched: true, Excerpt: text}
}

// synthesize asks the LLM to answer using only the fetched excerpts, cited by their source number.
func synthesize(c *gofr.Context, question string, sources []sourceResult) string {
	var ctx strings.Builder

	usable := 0
	for _, s := range sources {
		if !s.Fetched {
			continue
		}
		usable++
		ctx.WriteString(fmt.Sprintf("[%d] %s\n%s\n\n", s.N, s.URL, s.Excerpt))
	}

	if usable == 0 {
		return "None of the given sources could be fetched — see each source's refused/reason field."
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a research assistant. Answer the question using ONLY " +
			"the numbered sources below — never use outside knowledge. Cite sources inline as [n], " +
			"matching their number. If the sources don't contain the answer, say so plainly instead " +
			"of guessing."},
		{Role: ai.RoleUser, Content: "Sources:\n" + ctx.String() + "\nQuestion: " + question},
	}, ai.WithTemperature(0.2))
	if err != nil {
		return fmt.Sprintf("fetched %d source(s) but the model call failed: %v", usable, err)
	}

	return strings.TrimSpace(resp.Content)
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"')]+`)

// extractURLs pulls every distinct https?:// link out of free-form text, trimming trailing
// punctuation a sentence might leave attached (e.g. "...see https://example.com.").
func extractURLs(text string) []string {
	matches := urlRe.FindAllString(text, -1)

	out := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))

	for _, m := range matches {
		m = strings.TrimRight(m, ".,;:!?)")
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}

	return out
}

// isSafeURL is the guardrail: it runs BEFORE any outbound request (and again on every redirect),
// so a hostile or prompt-injected URL is refused deterministically — never left to the model's
// restraint. It blocks non-http(s) schemes, embedded credentials, localhost/internal/metadata
// hostnames, and literal loopback/private/link-local IPs (which covers the classic
// 169.254.169.254 cloud-metadata SSRF target).
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

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tagRe         = regexp.MustCompile(`(?s)<[^>]+>`)
	wsRe          = regexp.MustCompile(`\s+`)
)

// stripHTML turns a raw HTML page into plain, whitespace-collapsed text good enough to hand to the
// LLM as a source excerpt — no external dependency, just enough to drop markup and scripts/styles.
func stripHTML(body string) string {
	body = scriptStyleRe.ReplaceAllString(body, " ")
	body = tagRe.ReplaceAllString(body, " ")
	body = html.UnescapeString(body)
	body = wsRe.ReplaceAllString(body, " ")

	return strings.TrimSpace(body)
}
