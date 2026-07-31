package main

import (
	"strings"
	"testing"
)

// Behavioural tests for the intent guard, structured as CheckList (Ribeiro et al., ACL 2020): a
// Minimum Functionality Test per capability, an Invariance test for perturbations that must NOT
// change the label, and a Directional Expectation test for perturbations that must move it a known
// way. Accuracy on a held-out set is reported separately (TestIntentGuardOnRealPrompts) because
// aggregate accuracy hides exactly the failures that matter here.

func directive(t *testing.T, q string) bool {
	t.Helper()

	return looksDirective(q, NewConfig())
}

// MFT — the guard's core capability: commands out, information needs in.
func TestIntentMinimumFunctionality(t *testing.T) {
	commands := []string{
		"ping me once done",
		"kill the port-forwards and run it properly locally",
		"make it a visual one pager i can actually look at",
		"lets check for comments",
		"adhd response please",
		"yes post 1-3 as a follow-up comment",
		"mention umang and ask to take network",
		"too big make it ADHD",
		"yes that shape, do it in 2454",
		"dont ask umang like that mention my cinvers to aksaht",
		"we had 2 PRs, 2453 will have this thing and 2454 the frontend",
		"can you rerun the tests", // interrogative in SHAPE, directive in INTENT — the Cohen et al. case
		"/Users/raramuri/Downloads/report.zip summarise this",
	}

	for _, q := range commands {
		if !directive(t, q) {
			t.Errorf("MISSED a command, it will pollute the work list: %q", q)
		}
	}

	questions := []string{
		"what is our policy on deploying on fridays",
		"should we fail closed when the auth service is down",
		"how do we handle retries for non-idempotent writes",
		"which store is authoritative for orders",
		"why do we store money as int64 cents",
		"terraform remote state locking backend", // a bare noun phrase is a legitimate informational query
		"kubernetes ingress nginx annotations",
	}

	for _, q := range questions {
		if directive(t, q) {
			t.Errorf("DROPPED a genuine question — this loses a real gap silently: %q", q)
		}
	}
}

// MFT (adversarial) — the failure the literature predicts. Org policies are written imperatively;
// Cohen et al. score Request at 0.58 against Directive at 0.78 for exactly this reason ("positive and
// negative classes are not clearly separated"). An architectural imperative must survive, which is why
// the verb list holds only session/tooling verbs and never verbs that act on the system.
func TestImperativePolicyQueriesSurvive(t *testing.T) {
	for _, q := range []string{
		"fail closed on auth service errors",
		"add rate limiting server-side as middleware",
		"never log request bodies containing payment details",
		"retry failed calls with exponential backoff",
		"do not retry non-idempotent writes",
		"drain in-flight connections on graceful shutdown",
		"cache read-heavy endpoints behind a short TTL",
		"propagate context deadlines across internal calls",
	} {
		if directive(t, q) {
			t.Errorf("an imperative POLICY query was dropped as a command: %q", q)
		}
	}
}

// INV — the label must not change under perturbations that preserve intent. The user this was built
// for types fast: "reveiw", "iut", "fic it", "startegy" all appear verbatim in the live prompt log,
// so misspelling-invariance is a real requirement rather than a synthetic one.
func TestIntentInvariantToTypos(t *testing.T) {
	pairs := [][2]string{
		{"what is our review policy", "what is our reveiw policy"},
		{"how do we stress test it", "how do we stress test iut"},
		{"should we fail closed", "shoud we fail closed"},
		{"ping me once done", "ping me once donee"},
		{"lets check for comments", "lets chek for comments"},
	}

	for _, p := range pairs {
		if directive(t, p[0]) != directive(t, p[1]) {
			t.Errorf("a typo flipped the label: %q -> %v, %q -> %v",
				p[0], directive(t, p[0]), p[1], directive(t, p[1]))
		}
	}

	// Case and trailing punctuation must not matter either.
	base := "what is our deploy policy"
	for _, v := range []string{strings.ToUpper(base), base + "   ", "  " + base + "!!"} {
		if directive(t, base) != directive(t, v) {
			t.Errorf("surface form flipped the label: %q vs %q", base, v)
		}
	}
}

// DIR — perturbations that must move the label in a KNOWN direction. Prefixing an interrogative onto
// a bare phrase makes it a question; there is no perturbation that should turn a question into a
// command, which is the asymmetry the guard's ordering encodes.
func TestIntentDirectionalExpectation(t *testing.T) {
	for _, bare := range []string{"check the retry policy", "show the rate limits", "list the deploy rules"} {
		if !directive(t, bare) {
			t.Fatalf("setup: %q should read as a command", bare)
		}

		for _, prefix := range []string{"what is ", "should we ", "how do we "} {
			if directive(t, prefix+bare) {
				t.Errorf("adding %q did not turn a command into a question: %q", prefix, prefix+bare)
			}
		}
	}
}

// The held-out set: every gap the live service accumulated in normal use, hand-labelled. Real data,
// not synthetic — the distribution a synthetic set cannot reproduce is exactly the one that broke it.
//
// The target is PRECISION on questions, not accuracy. A surviving command costs one junk row; a
// dropped question silently loses a real gap and leaves no trace by construction. Cohen et al. report
// F1 0.72-0.78 on Directive with a trained classifier and note human annotators agree only
// kappa 0.72-0.83, so this asserts a realistic bar, not a perfect one.
func TestIntentGuardOnRealPrompts(t *testing.T) {
	type labelled struct {
		q  string
		is bool // true = directive/instruction, should be kept OFF the list
	}

	live := []labelled{
		{"what is our policy on deploying on fridays", false},
		{"terraform remote state locking backend", false},
		{"kill the port-forwards and run it properly locally", true},
		{"we had an 2 PRs 2453 will have this thing, and run 2453 backend and 2454 frontend db already runnning so I can actually test", true},
		{"make it a visual one pager i can actually look at", true},
		{"lets check for comments", true},
		{"adhd response please what's wrong?", true},
		{"yes post 1-3 as a follow-up comment", true},
		{"adhd response please", true},
		{"check for the org da470928-b034-4618-b39e-c947e82d30cd", true},
		{"mention umang and ask to take network", true},
		{"what did our own reveiws say?", true},
		{"new excel created", true},
		{"how much are wrong till now", true},
		{"yes that shape, do it in 2454", true},
		{"too big make it ADHD", true},
		{"this is not how we write we write in point4err adhd style", true},
		{"what is dto split", false}, // a genuine "what does this term mean" question
		{"what is dont what is left what is the gap", true},
		{"add encryption to 2494", true},
		{"ping me once done", true},
		{"dont ask umang like that mention my cinvers to aksaht and aks umngg to look", true},
	}

	var (
		correct        int
		droppedReal    []string // the expensive error: a question suppressed
		survivingNoise []string // the cheap error: a command that got through
	)

	for _, c := range live {
		got := looksDirective(c.q, NewConfig())

		switch {
		case got == c.is:
			correct++
		case got && !c.is:
			droppedReal = append(droppedReal, c.q)
		default:
			survivingNoise = append(survivingNoise, c.q)
		}
	}

	t.Logf("held-out: %d/%d correct (%.0f%%), %d questions wrongly dropped, %d commands survived",
		correct, len(live), 100*float64(correct)/float64(len(live)), len(droppedReal), len(survivingNoise))

	for _, q := range survivingNoise {
		t.Logf("  survived (cheap): %q", q)
	}

	// A wrongly-dropped question is the error that must not happen: it is invisible by construction.
	for _, q := range droppedReal {
		t.Errorf("  DROPPED A GENUINE QUESTION: %q", q)
	}

	// Before the guard the list was 22/24 noise. Anything short of a large majority is not worth the
	// complexity, so this pins the win rather than merely asserting the guard runs.
	if correct*100/len(live) < 75 {
		t.Errorf("only %d/%d correct — below the bar the prior work suggests is achievable", correct, len(live))
	}
}

// FuzzLooksDirective: the guard indexes into token slices (words[head+1]) and slices strings. It runs
// on unvalidated input from every prompt, so a panic here takes down recall for the whole session.
func FuzzLooksDirective(f *testing.F) {
	for _, s := range []string{"", " ", "?", "please", "dont", "lets", "yes", "/users/", "a?"} {
		f.Add(s)
	}

	cfg := NewConfig()

	f.Fuzz(func(_ *testing.T, s string) {
		_ = looksDirective(s, cfg)
	})
}
