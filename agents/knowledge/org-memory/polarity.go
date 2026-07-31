package main

import "strings"

// Polarity guard — refuse to answer a question with its own opposite.
//
// WHAT THIS FIXES. Measured, repeatedly, on both the real corpus and the committed fixture:
//
//	"should we fail CLOSED when the store is down"  ->  "Fail OPEN (allow the request)"
//	"do NOT retry failed requests"                  ->  "RETRY failed calls with backoff"
//	"DISABLE rate limiting"                         ->  "IMPLEMENT rate limiting server-side"
//
// Every one arrives with weak=false and noise_p ~1e-13 — the system is as certain as it is capable of
// being while advising the exact reverse of what was asked. That is the worst output this design can
// produce. An abstention is a correct answer here; a confident inversion is not.
//
// WHY NOTHING ELSE CAUGHT IT. A bi-encoder scores topic, and these pairs are topically identical —
// one token apart in an otherwise matching sentence. Three separate attempts failed on measurement:
// the BM25 hybrid made negation WORSE (it weights the shared content terms above the low-idf
// negation, pulling the polarities together); a cross-encoder separates them 6.5x when both sides are
// present but scored the inverse 0.97 when only the inverse existed, because it scores pairs
// independently and has no notion of "none of these"; and no confidence signal fires, because the
// retrieval is not confused — it is confidently serving the nearest thing, which happens to be the
// opposite.
//
// WHAT THIS IS AND IS NOT. This is a GUARD, not a solution. It is deliberately narrow and lexical: it
// only fires when a decision asserts the OPPOSITE polarity of something the query explicitly asked
// about. It cannot understand intent, it will miss paraphrased inversions, and the real fix remains a
// model that reads query and document together (Weller et al., NevIR, EACL 2024). What it does buy is
// the one property that matters most here: the failure mode degrades from "confidently wrong" to
// "silent", and silence is a correct output in this system.
//
// Both the pair list and the negation cues are config (Gate #0) — which words are opposites is a
// property of a domain's vocabulary, not of this engine.

// defaultPolarityPairs are opposing terms whose confusion inverts advice. Each line is one pair.
const defaultPolarityPairs = "open|closed,enable|disable,allow|deny,on|off,always|never," +
	"add|remove,start|stop,include|exclude,grant|revoke,accept|reject," +
	"implement|disable,sync|async,push|pull"

// defaultNegationCues mark the token that follows as asserted in the NEGATIVE.
const defaultNegationCues = "not,no,never,dont,doesnt,without,avoid,skip,stop,disable,drop"

// polarityConflict reports whether `doc` asserts the opposite of what `query` asked.
//
// Two independent mechanisms, because inversion shows up two different ways:
//
//   - ANTONYM PAIRS — "fail closed" vs "fail open". The polarity lives in a content word, and the
//     two sides are different tokens entirely.
//   - NEGATION SCOPE — "do not retry" vs "retry". The polarity lives in a cue word attached to a
//     SHARED token, so lexical overlap is maximal and every similarity measure is blind to it.
func polarityConflict(query, doc string, cfg *Config, chain ...string) bool {
	q := lexTokens(strings.ToLower(query))
	d := lexTokens(strings.ToLower(doc))

	if len(q) == 0 || len(d) == 0 {
		return false
	}

	if antonymConflict(q, d, cfg, chain...) {
		return true
	}

	return negationConflict(q, d, cfg, chain...)
}

// antonymConflict fires when the query asserts one side of a known pair and the doc asserts the other
// — and the doc does NOT also assert the query's side, since a decision covering both ("fail open on
// cache errors, fail closed on auth errors") genuinely answers either question.
func antonymConflict(q, d []string, cfg *Config, chain ...string) bool {
	qs, ds := set(q), set(d)

	for _, pair := range strings.Split(cfg.Str("rank.polarity_pairs", defaultPolarityPairs, chain...), ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(pair), "|")
		if !ok {
			continue
		}

		a, b = strings.TrimSpace(a), strings.TrimSpace(b)

		if qs[a] && ds[b] && !ds[a] {
			return true
		}

		if qs[b] && ds[a] && !ds[b] {
			return true
		}
	}

	return false
}

// negationConflict fires when a token the query NEGATES is asserted affirmatively by the doc.
//
// Scope is the two tokens following a cue, which is where negation lands in the terse imperative
// style decisions are written in ("do not retry non-idempotent writes"). A wider window starts
// swallowing unrelated clauses and turns the guard into a nuisance.
func negationConflict(q, d []string, cfg *Config, chain ...string) bool {
	cues := set(strings.Split(cfg.Str("rank.negation_cues", defaultNegationCues, chain...), ","))
	span := cfg.I("rank.negation_scope", 2, chain...)

	qNeg := negatedTokens(q, cues, span)
	if len(qNeg) == 0 {
		return false
	}

	dNeg := negatedTokens(d, cues, span)

	for _, tok := range d {
		// The doc asserts `tok` affirmatively; the query asked for its absence.
		if qNeg[tok] && !dNeg[tok] {
			return true
		}
	}

	return false
}

// negatedTokens returns the tokens falling within `span` of a negation cue.
func negatedTokens(toks []string, cues map[string]bool, span int) map[string]bool {
	out := map[string]bool{}

	for i, t := range toks {
		if !cues[t] {
			continue
		}

		for j := i + 1; j <= i+span && j < len(toks); j++ {
			if !cues[toks[j]] {
				out[toks[j]] = true
			}
		}
	}

	return out
}

func set(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}

	return out
}

// entityConflict reports whether `scope` names a DIFFERENT subject than the one the query asked
// about — a decision about `table:order_events` cannot answer a question that says "orders".
//
// This is the other half of the same failure, and the more stubborn one. Two decisions can both be
// TRUE and still make each other wrong answers, because they are about different things: "MySQL
// order_events is the source of truth" and "ClickHouse is the sole store for orders" are each
// correct, topically near-identical, and mutually useless. Dense retrieval conflates them; BM25 could
// not separate them (the query says "orders", the decision says "order_events" — no lexical method
// bridges that); a cross-encoder could not either.
//
// What works is not inference at all: the corpus ALREADY CARRIES the answer as a facet. A decision
// tagged `table:orders` is a statement about which subject it governs. So when a query names a
// subject some decision is faceted on, decisions faceted on a DIFFERENT value of that same facet are
// simply not about it. Structure the corpus already has beats a model guessing from prose.
//
// Deliberately narrow: it only fires when the query explicitly names the entity, and never when the
// decision also carries the queried value (a decision genuinely about both is a valid answer).
func entityConflict(queryTokens map[string]bool, scope []string, entityPrefixes []string) bool {
	for _, s := range scope {
		prefix, value, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), ":")
		if !ok || value == "" {
			continue
		}

		if !containsStr(entityPrefixes, prefix+":") {
			continue
		}

		// The decision is about `value`. If the query named a DIFFERENT value of this same facet and
		// not this one, the decision is about a different subject.
		if !queryTokens[value] {
			return true
		}
	}

	return false
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}

	return false
}

// queryNamesEntity reports whether the query mentions any value of an entity facet present in the
// corpus — the precondition for entityConflict to mean anything. Without it, every scoped decision
// would be dropped for every unscoped query.
func queryNamesEntity(queryTokens map[string]bool, corpusEntityValues map[string]bool) bool {
	for tok := range queryTokens {
		if corpusEntityValues[tok] {
			return true
		}
	}

	return false
}
