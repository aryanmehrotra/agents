package main

import "strings"

// Intent guard — keep instructions off the corpus's work list.
//
// WHAT THIS FIXES. Once the recall hook was attached to every prompt, the gap list — which exists to
// name decisions the org should write down — filled with things said TO an assistant rather than
// asked ABOUT the org. Measured on the live list: 22 of 24 entries, 92%.
//
//	kill the port-forwards and run it properly locally
//	adhd response please
//	ping me once done
//	yes post 1-3 as a follow-up comment
//
// This is not tidiness. The gap list feeds calibration: recallHealth watches the near-miss rate and
// ask.go surfaces gaps as questions to a human. A polluted list spends the user's scarce attention
// asking them to write down a rule about "ping me once done", and the ask budget only works while
// they still answer. Observed already: 4 helpful against 7 not_relevant and 8 wrong.
//
// WHY THIS IS A KNOWN TASK, NOT AN INVENTED TAXONOMY. Separating a command from an information need
// is dialogue act classification, and the split is Searle's: a DIRECTIVE ("do this") against an
// information-seeking act ("what do we do?"). Statement / Question / Directive is a standard tag set
// (Stolcke et al., Computational Linguistics 26(3), 2000, building on DAMSL), not a local invention.
// The framing of "most inputs belong to no supported class" is out-of-scope intent detection (Larson
// et al., EMNLP 2019).
//
// WHAT THE CLOSEST PRIOR WORK MEASURED. Cohen, Carvalho & Mitchell, "Learning to Classify Email into
// Speech Acts" (EMNLP 2004) is the same problem on a different corpus — 1,357 messages, Searle-derived
// classes. Three of their results shaped this file:
//
//   - DIRECTIVE is their BEST class: F1 0.72-0.78 (DT 0.78, SVM/AB 0.73, VP 0.72), against 0.58-0.69
//     for Request. The act worth detecting here is the one that detects best.
//   - BIGRAMS beat TF-IDF, and common words beat rare ones: "the most discriminating words for most
//     of these categories were common words". Their example is exact — "will you" and "I will" predict
//     requests and commitments while the individual words do not. Hence the bigram cues below.
//   - Inter-annotator kappa is 0.72-0.83. HUMANS disagree about a fifth of the time, so ~0.78 is near
//     the ceiling for this task and any claim of near-perfect separation here would be false.
//
// WHERE THIS WILL FAIL, PREDICTED BY THAT SAME PAPER. Org policies are written imperatively: "fail
// closed on auth errors" and "add rate limiting server-side" are grammatically commands. Cohen et al.
// hit the same wall — it is why Request scores 0.58 while Directive scores 0.78 — and they name the
// cause: "positive and negative classes are not clearly separated". The mitigation here is that the
// verb list holds only verbs that act on the SESSION or its tooling (ping, rerun, commit, post), never
// verbs that act on system architecture (fail, retry, cache, add, enable). An architectural imperative
// stays on the list.
//
// OPERATING POINT. High precision at reduced recall, which is the operating point Cohen et al.
// explicitly recommend: "high-precision predictions can be made ... for verbs Request and Deliver, if
// one is willing to slightly reduce recall." Concretely, when a phrase is ambiguous this KEEPS it —
// a surviving instruction costs one junk row, while a dropped question silently loses a real gap and
// leaves no trace that it happened. Every drop is counted (see gapLog.Rejected) so the cost is
// auditable rather than invisible.
//
// Every list below is config (Gate #0). Which verbs are session chatter is a property of how a given
// team talks to its tools, not of this engine.

const (
	// defaultDirectiveBigrams are two-word cues that mark a command regardless of surrounding form.
	// These carry the load, per Cohen et al.: "can you run the tests?" is interrogative in shape and
	// directive in intent, and no single word in it says so.
	defaultDirectiveBigrams = "make it,do it,give me,show me,can you,could you,would you,i want," +
		"i need,let me,send me,tell me,write me,for me"

	// defaultSessionVerbs are imperative verbs that act on the SESSION or its tooling. Deliberately
	// excludes architectural verbs (add, create, enable, disable, remove, retry, cache, fail) — those
	// appear in genuine policy questions, which is exactly the failure mode the literature predicts.
	// `retry` and `write` were here and had to come out: "retry failed calls with exponential backoff"
	// and "write middleware with the standard handler signature" are both real policies in the fixture
	// corpus. That is precisely the collision Cohen et al. predict, caught by the adversarial MFT.
	defaultSessionVerbs = "ping,rerun,run,kill,restart,commit,push,merge,rebase,revert," +
		"post,reply,send,mention,tell,ask,check,look,read,draft,summarize,summarise,explain," +
		"list,show,give,make,fix,rename,paste,print,continue,proceed,stop,wait,undo,redo"

	// defaultInterrogatives open a genuine information need.
	defaultInterrogatives = "what,why,how,when,where,which,who,whose,whom,should,can,could,do,does," +
		"did,is,are,was,were,will,would,has,have,am,any,anyone"

	// defaultDiscourseMarkers are throat-clearing that precedes the real head of the utterance
	// ("yes post 1-3 as a follow-up"). Stripped before the leading token is examined.
	defaultDiscourseMarkers = "yes,yeah,yep,ok,okay,no,nope,sure,also,and,then,now,so,but,well," +
		"please,just,actually,hey,hi,alright"

	// defaultDemonstratives open a STATEMENT about the session rather than any question — "this is not
	// how we write", "new excel created". Statements are the third DAMSL class (Stolcke et al., 2000)
	// and belong on the work list no more than commands do.
	defaultDemonstratives = "this,that,these,those,it,its,here,there"

	// defaultFirstPerson opens narration about the session rather than a question about the org
	// ("we had 2 PRs, 2453 will have this thing").
	defaultFirstPerson = "i,we,my,me,mine,us"
)

// looksDirective reports whether `q` is something said TO an assistant rather than asked ABOUT the
// org. True means "keep this off the work list".
//
// Ordered so the strongest evidence wins. Bigram cues are checked BEFORE interrogative openers
// precisely because "can you rerun it?" is a question in shape and a command in intent — the case
// Cohen et al. single out as the reason bigrams beat unigrams here.
func looksDirective(q string, cfg *Config, chain ...string) bool {
	s := strings.ToLower(strings.TrimSpace(q))
	if s == "" {
		return false
	}

	// A filesystem path is a handoff of material to work on, never a question about a decision.
	if strings.Contains(s, "/users/") || strings.Contains(s, "~/") || strings.Contains(s, ".com/") {
		return true
	}

	for _, bg := range splitCfgList(cfg.Str("gaps.directive_bigrams", defaultDirectiveBigrams, chain...)) {
		if strings.Contains(s, bg) {
			return true
		}
	}

	words := wordsOf(s)
	if len(words) == 0 {
		return false
	}

	// "please" and "lets" mark a directive wherever they land.
	for _, w := range words {
		if w == "please" || w == "lets" || w == "let's" {
			return true
		}
	}

	markers := set(splitCfgList(cfg.Str("gaps.discourse_markers", defaultDiscourseMarkers, chain...)))

	head := 0
	for head < len(words) && markers[words[head]] {
		head++
	}

	if head >= len(words) {
		return true // nothing but throat-clearing
	}

	// An interrogative head is an information need — stop here, before the verb list can misfire on a
	// question that happens to contain a session verb ("what should we check before a release?").
	if set(splitCfgList(cfg.Str("gaps.interrogatives", defaultInterrogatives, chain...)))[words[head]] {
		return false
	}

	if strings.HasSuffix(s, "?") {
		return false
	}

	// A demonstrative head opens a statement, not a question.
	if set(splitCfgList(cfg.Str("gaps.demonstratives", defaultDemonstratives, chain...)))[words[head]] {
		return true
	}

	// A bare ticket, PR or org id is a reference to session material — "add encryption to 2494". Only
	// reachable for non-interrogative text, so "what is our timeout for 8080" has already returned.
	if hasBareID(words) {
		return true
	}

	verbs := set(splitCfgList(cfg.Str("gaps.session_verbs", defaultSessionVerbs, chain...)))
	if verbs[words[head]] {
		return true
	}

	// A negated command is still a command: "dont ask umang like that".
	if head+1 < len(words) &&
		set(strings.Split(cfg.Str("rank.negation_cues", defaultNegationCues, chain...), ","))[words[head]] &&
		verbs[words[head+1]] {
		return true
	}

	// First-person narration about the session.
	return set(splitCfgList(cfg.Str("gaps.first_person", defaultFirstPerson, chain...)))[words[head]]
}

// wordsOf splits on whitespace and strips surrounding punctuation, PRESERVING ORDER — unlike
// lexTokens, which drops stopwords and is therefore useless for "what is the leading word".
func wordsOf(s string) []string {
	out := make([]string, 0, 8)

	for _, f := range strings.Fields(s) {
		w := strings.Trim(f, ".,!?;:\"'()[]{}")
		if w != "" {
			out = append(out, w)
		}
	}

	return out
}

func splitCfgList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))

	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// hasBareID reports whether any token is a standalone identifier — a 4+ digit ticket number or a
// hyphenated hex id. Deliberately reached only after the interrogative check, because "what retry
// budget do we use at 3000 rps" is a genuine question that happens to contain a number.
func hasBareID(words []string) bool {
	for _, w := range words {
		digits, hyphens, other := 0, 0, 0

		for i := 0; i < len(w); i++ {
			switch {
			case w[i] >= '0' && w[i] <= '9':
				digits++
			case w[i] == '-':
				hyphens++
			default:
				other++
			}
		}

		if other == 0 && digits >= 4 {
			return true // a bare ticket number
		}

		if hyphens >= 3 && digits >= 8 {
			return true // a uuid-shaped id
		}
	}

	return false
}
