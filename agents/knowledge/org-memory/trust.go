package main

import (
	"net"
	"net/http"
	"strings"
	"unicode"
)

// This file closes the Tier-3 red-team's HIGH finding, which was not a ranking bug.
//
// The finding: `/capture` is unauthenticated, and everything an attacker writes into `what`, `why`
// and `source` is rendered BYTE-VERBATIM into the agent-facing `guidance` string — including the
// `[src: …]` marker, which reads as a provenance attestation. A plausible-but-wrong decision took
// rank #1 with `weak:false` on all four test queries.
//
// The important nuance from that same red-team: the poison could NOT rank-boost itself. `prior_cap`
// held, so authority and recency were unabusable; the attack won on raw cosine by mirroring the
// query, which no ranking change can fix — a decision that genuinely is the best lexical match for a
// question is *supposed* to rank first. That is why the fix belongs at ADMISSION (who may write) and
// PRESENTATION (how untrusted text is framed), never at ranking. Hardening the ranker here would
// have degraded honest retrieval while leaving the hole open.

// writeGuard decides whether a mutating request is allowed.
//
// Policy, in order:
//   - a configured write token must match the `X-Org-Memory-Token` header (or `?token=`);
//   - with NO token configured, writes are accepted only from loopback.
//
// That keeps the zero-config local experience the project is built around (convention over
// configuration — it must run with an empty config) while making the default non-exploitable from
// off-box. This is the plan's own documented next step: "bind-to-loopback + a write token".
func writeGuard(cfg *Config, r *http.Request) (string, bool) {
	want := strings.TrimSpace(cfg.Str("security.write_token", ""))

	if want != "" {
		got := strings.TrimSpace(r.Header.Get("X-Org-Memory-Token"))
		if got == "" {
			got = strings.TrimSpace(r.URL.Query().Get("token"))
		}

		// Length-independent comparison is not required here (the token is not a secret derived from
		// user input and the endpoint is not an oracle), but constant-time is free via subtle if this
		// ever fronts the public internet — see security.md TODO.
		if got != want {
			return "write requires a valid X-Org-Memory-Token", false
		}

		return "", true
	}

	if isLoopback(r.RemoteAddr) {
		return "", true
	}

	return "writes are restricted to loopback until `security.write_token` is configured", false
}

// isLoopback reports whether addr ("host:port" or bare host) is a loopback address.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}

	if host == "" || host == "localhost" {
		return host == "localhost"
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// sanitizeAdvice neutralises captured text before it is placed into an agent's context.
//
// Captured text is DATA that an agent reads as part of its prompt, so it inherits the whole
// prompt-injection surface. The specific lever this removes is STRUCTURE: control characters and
// newlines let a stored decision fabricate what looks like a message boundary, a new system turn, or
// a fake tool result. Collapsing everything to single spaces means a decision can only ever be one
// line of text — it can still SAY something false (no sanitiser fixes that; that is what provenance
// and the correctness layer are for) but it cannot pretend to be a different KIND of content.
//
// Length is capped for the same reason it is capped in the ranker: injected context has a cost, and
// an unbounded `why` is both a context-flooding vector and a denial-of-wallet one.
func sanitizeAdvice(s string, maxRunes int) string {
	var b strings.Builder

	b.Grow(len(s))

	lastSpace := false

	for _, r := range s {
		switch {
		case r == '�' || unicode.Is(unicode.Cf, r):
			continue // format/bidi controls — invisible reordering tricks
		case unicode.IsControl(r) || unicode.IsSpace(r):
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			b.WriteRune(r)

			lastSpace = false
		}
	}

	out := strings.TrimSpace(b.String())
	if maxRunes <= 0 {
		return out
	}

	rs := []rune(out)
	if len(rs) <= maxRunes {
		return out
	}

	return strings.TrimSpace(string(rs[:maxRunes])) + "…"
}

// trustedProvenance reports whether a decision's provenance is on the org's verified list.
//
// The `[src: …]` marker is an ATTESTATION — it tells the reading agent "this came from somewhere
// real". An attacker who can write a decision can also write its `source`, so an unconditional
// marker lets untrusted input forge the very signal that makes it trustworthy. Gating the marker on
// an org-configured provenance allowlist means the attestation can only be produced by the org, not
// by the content. Empty allowlist ⇒ nothing is attested, which is the correct fail-closed default:
// an unconfigured deployment should under-claim, not over-claim.
func trustedProvenance(prov string, cfg *Config, chain ...string) bool {
	prov = strings.ToLower(strings.TrimSpace(prov))
	if prov == "" {
		return false
	}

	for _, allowed := range strings.Split(cfg.Str("trust.verified_provenance", "", chain...), ",") {
		a := strings.ToLower(strings.TrimSpace(allowed))
		if a == "" {
			continue
		}

		if a == prov || (strings.HasSuffix(a, "*") && strings.HasPrefix(prov, strings.TrimSuffix(a, "*"))) {
			return true
		}
	}

	return false
}
