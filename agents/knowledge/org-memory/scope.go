package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"
)

// idFor derives a stable content ID from what+scope, so re-recording the same knowledge
// reinforces the same decision (dedupe) instead of duplicating it.
func idFor(what string, scope []string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(what)) + "|" + normScope(scope)))

	return "d-" + hex.EncodeToString(sum[:])[:12]
}

// normScope canonicalises scope tags (lowercased, trimmed, sorted, deduped) so equivalent
// scopes hash identically regardless of order or casing.
func normScope(scope []string) string {
	seen := map[string]bool{}

	var tags []string

	for _, s := range scope {
		t := strings.ToLower(strings.TrimSpace(s))
		if t == "" || seen[t] {
			continue
		}

		seen[t] = true
		tags = append(tags, t)
	}

	sort.Strings(tags)

	return strings.Join(tags, "|")
}

// scopeTags returns the tag set of a scope; empty and "global" contribute nothing (match-any).
func scopeTags(scope []string) map[string]bool {
	tags := map[string]bool{}

	for _, s := range scope {
		t := strings.ToLower(strings.TrimSpace(s))
		if t == "" || t == "global" {
			continue
		}

		tags[t] = true
	}

	return tags
}

// scopeMatch reports whether a decision applies in a context, and how specific the match is.
// A decision matches when it is global (no tags) or when every one of its tags is present in the
// context (its scope is a subset of where you are). Specificity = number of matched tags, so a
// repo/service-scoped rule out-ranks a global one (most-specific-wins).
func scopeMatch(decisionScope []string, context map[string]bool) (ok bool, specificity int) {
	tags := scopeTags(decisionScope)
	if len(tags) == 0 {
		return true, 0
	}

	for t := range tags {
		if !context[t] {
			return false, 0
		}
	}

	return true, len(tags)
}

// isScopeTag reports whether a string is a scope tag (key:value, no whitespace) rather than free-text
// query prose. Scope tags are used for matching; prose is used for the embedding.
func isScopeTag(s string) bool {
	s = strings.TrimSpace(s)

	return strings.Contains(s, ":") && !strings.ContainsAny(s, " \t")
}

// alwaysHardPrefixes are facets that act as HARD predicates unconditionally — not configurable, not
// disableable, not subject to `retrieve.hard_scope`.
//
// These are ISOLATION boundaries, not relevance hints. A result from the wrong tenant is not a
// less-relevant result, it is a data leak, and the difference between those two things must not be
// expressible as a tuning parameter. Saltzer & Schroeder (1975) name the two principles this encodes:
// *fail-safe defaults* (the base case is denial, and isolation holds without anyone configuring it)
// and *complete mediation* (every request is checked, with no path that skips the check). Leaving
// tenancy to the soft-scope prior meant a config write — `retrieve.hard_scope=0`, which is the
// DEFAULT — silently turned an isolation boundary into a ranking nudge, and the red-team duly found
// sandbox data surfacing at rank #1 for another repo's query.
var alwaysHardPrefixes = []string{"org:", "tenant:"}

// defaultHardFacetPrefixes are the facet kinds that act as hard predicates by convention: a wrong
// value is simply a wrong answer, so the item is excluded rather than down-weighted. Identity and
// type facets that the CALLER explicitly asserts (Filtered-DiskANN, Gollapudi et al., WWW 2023 —
// predicates belong in the search, not after it; Hearst, CACM 2006 — facets as correctness
// constraints rather than browsing aids). Topical scope (repo:/service:/topic:) stays a SOFT prior,
// deliberately, so a decision made in one repo can still surface in another when it genuinely applies.
const defaultHardFacetPrefixes = "author:,kind:"

// hardFacetPrefixes resolves the hard-predicate facet list: the always-hard isolation facets, plus
// whatever the org configures. Configurable because which facets are "a wrong value = a wrong
// answer" is a property of the org's tagging convention, not of this engine (Gate #0) — e.g. a corpus
// where `table:` names a subject entity should treat it as hard, which is not knowable here.
func hardFacetPrefixes(cfg *Config, chain ...string) []string {
	out := append([]string(nil), alwaysHardPrefixes...)

	for _, p := range strings.Split(cfg.Str("retrieve.hard_facet_prefixes", defaultHardFacetPrefixes, chain...), ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}

		if !strings.HasSuffix(p, ":") {
			p += ":" // tolerate "table" for "table:" — the colon is syntax, not a decision
		}

		out = append(out, p)
	}

	return out
}

// hardFilterTags picks the hard-predicate facets out of a query's tags (lowercased).
func hardFilterTags(tags []string, cfg *Config, chain ...string) []string {
	prefixes := hardFacetPrefixes(cfg, chain...)

	var out []string

	for _, s := range tags {
		t := strings.ToLower(strings.TrimSpace(s))
		for _, p := range prefixes {
			if strings.HasPrefix(t, p) {
				out = append(out, t)

				break
			}
		}
	}

	return out
}

// isolationOK enforces tenancy from the DATA side, which is the direction that actually contains a
// leak.
//
// hasAllTags checks the facets the QUERY asserts — it answers "did the caller ask for author:vikash
// and is this his?". That is a correctness filter, and it is useless for isolation, because a caller
// who simply omits the tenant tag asserts nothing and therefore filters nothing. Tenancy has to work
// the other way round: a decision that CARRIES an isolation facet is invisible unless the query
// carries the matching one. Untagged decisions stay shared, so a single-tenant deployment needs no
// configuration at all — but the moment data is labelled, the label binds.
//
// This is fail-safe defaults (Saltzer & Schroeder 1975): the absence of an assertion denies access
// rather than granting it. The previous behaviour was the inverse — sandbox decisions surfaced at
// rank #1 for an unrelated repo's query, because scope was a soft prior and an absent tag simply
// meant "no preference".
func isolationOK(scope []string, queryTags map[string]bool) bool {
	for _, s := range scope {
		t := strings.ToLower(strings.TrimSpace(s))

		for _, p := range alwaysHardPrefixes {
			if !strings.HasPrefix(t, p) {
				continue
			}

			if !queryTags[t] {
				return false // labelled data, unlabelled (or wrongly labelled) request
			}

			break
		}
	}

	return true
}

// lowerTagSet normalises a query's tags into a lookup set.
func lowerTagSet(tags []string) map[string]bool {
	out := make(map[string]bool, len(tags))
	for _, t := range tags {
		out[strings.ToLower(strings.TrimSpace(t))] = true
	}

	return out
}

// hasAllTags reports whether a decision's scope contains every required tag (case-insensitive). An
// empty requirement set matches everything.
func hasAllTags(scope, required []string) bool {
	if len(required) == 0 {
		return true
	}

	have := map[string]bool{}
	for _, s := range scope {
		have[strings.ToLower(strings.TrimSpace(s))] = true
	}

	for _, r := range required {
		if !have[r] {
			return false
		}
	}

	return true
}

// hasAlnum reports whether s contains at least one letter or digit — i.e. real query content, as
// opposed to blank/whitespace/punctuation-only input that would embed to a meaningless vector.
func hasAlnum(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}

	return false
}

// contextSet builds the lowercased tag set a recall runs against.
func contextSet(list []string) map[string]bool {
	set := map[string]bool{}

	for _, s := range list {
		if t := strings.ToLower(strings.TrimSpace(s)); t != "" && t != "global" {
			set[t] = true
		}
	}

	return set
}
