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

// hardFilterPrefixes are the facet kinds that act as HARD predicates (a wrong value = a wrong result),
// as opposed to soft topical priors. Identity/provenance facets the caller explicitly asserts.
var hardFilterPrefixes = []string{"author:", "kind:"}

// hardFilterTags picks the hard-predicate facets out of a query's tags (lowercased).
func hardFilterTags(tags []string) []string {
	var out []string

	for _, s := range tags {
		t := strings.ToLower(strings.TrimSpace(s))
		for _, p := range hardFilterPrefixes {
			if strings.HasPrefix(t, p) {
				out = append(out, t)

				break
			}
		}
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
