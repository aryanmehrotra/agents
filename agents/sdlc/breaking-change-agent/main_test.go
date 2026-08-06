package main

import (
	"strings"
	"testing"
)

// sample diff: user.go had GetUser removed and Age's type changed.
const sampleDiff = `diff --git a/user.go b/user.go
index 111..222 100644
--- a/user.go
+++ b/user.go
@@ -10,10 +10,7 @@ package user
 type User struct {
 Name string
-Age  int
+Age  string
 }

-func GetUser(id string) (*User, error) {
-return db.Find(id)
-}

 func stillHere() {}
`

// TestParseDiff extracts the added/removed lines with correct file + line numbers from a real hunk.
func TestParseDiff(t *testing.T) {
	lines := parseDiff(sampleDiff)
	if len(lines) == 0 {
		t.Fatal("parseDiff found no changed lines")
	}

	idx := diffLineIndex(lines)

	// "-	Age  int" is line 12 of the OLD file (10, 11=type User struct{, 12=Name string is 11... let's
	// just assert the removed/added Age lines and the removed GetUser line landed somewhere sane).
	var sawRemovedAge, sawAddedAge, sawRemovedGetUser bool

	for _, l := range lines {
		if l.File != "user.go" {
			t.Errorf("parseDiff file = %q, want user.go", l.File)
		}

		switch {
		case l.Kind == '-' && strings.Contains(l.Text, "Age  int"):
			sawRemovedAge = true
		case l.Kind == '+' && strings.Contains(l.Text, "Age  string"):
			sawAddedAge = true
		case l.Kind == '-' && strings.Contains(l.Text, "func GetUser"):
			sawRemovedGetUser = true
		}
	}

	if !sawRemovedAge || !sawAddedAge || !sawRemovedGetUser {
		t.Fatalf("parseDiff missed expected lines: removedAge=%v addedAge=%v removedGetUser=%v",
			sawRemovedAge, sawAddedAge, sawRemovedGetUser)
	}

	if len(idx) == 0 {
		t.Error("diffLineIndex should not be empty for a real diff")
	}
}

// TestParseDiffNoHunks returns nothing for text that isn't a unified diff — the handler uses this to
// refuse before ever calling the model.
func TestParseDiffNoHunks(t *testing.T) {
	if lines := parseDiff("just some prose\nno diff markers here\n"); len(lines) != 0 {
		t.Errorf("parseDiff(non-diff) = %v, want none", lines)
	}
}

// TestNormalizeFindingsVerifiesAgainstRealLines is the core guarantee: a finding whose file:line is a
// real changed line is verified; one that points at a line never touched by the diff is not, and
// neither counts toward invalid unless its category is unrecognized.
func TestNormalizeFindingsVerifiesAgainstRealLines(t *testing.T) {
	lines := parseDiff(sampleDiff)
	idx := diffLineIndex(lines)

	// Find the real line number the removed GetUser line landed on, so the "verified" case is exact.
	var realLine int

	for _, l := range lines {
		if l.Kind == '-' && strings.Contains(l.Text, "func GetUser") {
			realLine = l.Line
		}
	}

	raw := []map[string]any{
		{"file": "user.go", "line": float64(realLine), "category": "removed_symbol", "symbol": "GetUser", "description": "removed"},
		{"file": "user.go", "line": float64(9999), "category": "removed_symbol", "symbol": "Ghost", "description": "hallucinated line"},
		{"file": "user.go", "line": float64(realLine), "category": "not_a_real_category", "description": "bogus"},
	}

	verified, unverified, invalid := normalizeFindings(raw, idx)

	if len(verified) != 1 || verified[0].Symbol != "GetUser" || verified[0].Severity != "breaking" {
		t.Fatalf("verified = %#v, want exactly the GetUser finding marked breaking", verified)
	}

	if len(unverified) != 1 || unverified[0].Symbol != "Ghost" {
		t.Fatalf("unverified = %#v, want exactly the hallucinated-line finding", unverified)
	}

	if len(invalid) != 1 || invalid[0].Category != "not_a_real_category" {
		t.Fatalf("invalid = %#v, want exactly the unrecognized-category finding", invalid)
	}
}

// TestCategorySeverityIsClosed makes sure every category the prompt promises the model is actually in
// the table, and that advisory categories don't silently become breaking (or vice versa).
func TestCategorySeverityIsClosed(t *testing.T) {
	breaking := []string{"removed_symbol", "signature_changed", "type_changed", "required_field_added",
		"removed_enum_value", "visibility_reduced", "renamed_symbol"}
	advisory := []string{"behavior_changed", "deprecated", "other"}

	for _, c := range breaking {
		if categorySeverity[c] != "breaking" {
			t.Errorf("category %q severity = %q, want breaking", c, categorySeverity[c])
		}
	}

	for _, c := range advisory {
		if categorySeverity[c] != "advisory" {
			t.Errorf("category %q severity = %q, want advisory", c, categorySeverity[c])
		}
	}
}

// TestVerdict is the arithmetic the model is never trusted with: BREAKING iff at least one verified
// finding is a breaking category; an advisory-only set of findings is SAFE.
func TestVerdict(t *testing.T) {
	safe := []finding{{Severity: "advisory"}, {Severity: "advisory"}}
	if breaking, n := verdict(safe); breaking || n != 0 {
		t.Errorf("verdict(advisory-only) = %v, %d; want false, 0", breaking, n)
	}

	mixed := []finding{{Severity: "advisory"}, {Severity: "breaking"}, {Severity: "breaking"}}
	if breaking, n := verdict(mixed); !breaking || n != 2 {
		t.Errorf("verdict(mixed) = %v, %d; want true, 2", breaking, n)
	}

	if breaking, n := verdict(nil); breaking || n != 0 {
		t.Errorf("verdict(nil) = %v, %d; want false, 0", breaking, n)
	}
}

// TestExtractFindings pulls the findings array out of a fenced/prose-wrapped response and separately
// sniffs a model-volunteered top-level verdict without letting it leak into the findings themselves.
func TestExtractFindings(t *testing.T) {
	resp := "here you go:\n```json\n" +
		`{"verdict":"breaking","findings":[{"file":"a.go","line":1,"category":"removed_symbol"}]}` +
		"\n```"

	findings, claimed, err := extractFindings(resp)
	if err != nil {
		t.Fatalf("extractFindings error: %v", err)
	}

	if len(findings) != 1 || findings[0]["file"] != "a.go" {
		t.Fatalf("extractFindings findings = %#v", findings)
	}

	if claimed == nil || *claimed != "breaking" {
		t.Fatalf("extractFindings claimedVerdict = %v, want \"breaking\"", claimed)
	}
}

// TestExtractFindingsNoVerdict confirms a well-behaved response (no volunteered verdict) leaves the
// claimed verdict nil rather than inventing one.
func TestExtractFindingsNoVerdict(t *testing.T) {
	_, claimed, err := extractFindings(`{"findings":[]}`)
	if err != nil {
		t.Fatalf("extractFindings error: %v", err)
	}

	if claimed != nil {
		t.Errorf("claimedVerdict = %v, want nil when the model gave none", *claimed)
	}
}

// TestExtractJSONNoObject errors cleanly on a response with no JSON at all.
func TestExtractJSONNoObject(t *testing.T) {
	if _, err := extractJSON("no json here"); err == nil {
		t.Error("extractJSON(no object) should error")
	}
}

// TestDiffPath strips the git a/ b/ prefix and trailing timestamp noise from a diff header.
func TestDiffPath(t *testing.T) {
	if got := diffPath("a/pkg/user.go\t2026-08-06", "a/"); got != "pkg/user.go" {
		t.Errorf("diffPath = %q, want pkg/user.go", got)
	}

	if got := diffPath("/dev/null", "a/"); got != "/dev/null" {
		t.Errorf("diffPath(/dev/null) = %q, want unchanged", got)
	}
}

// TestIntFrom reads a JSON number and safely zeroes anything else instead of panicking.
func TestIntFrom(t *testing.T) {
	if intFrom(float64(42)) != 42 {
		t.Error("intFrom(float64) should read the int value")
	}

	if intFrom("not a number") != 0 {
		t.Error("intFrom(non-number) should be 0, not panic")
	}
}

// TestCleanField truncates an over-long value with an ellipsis.
func TestCleanField(t *testing.T) {
	if got := cleanField(strings.Repeat("x", maxFieldLen+50)); !strings.HasSuffix(got, "…") {
		t.Errorf("cleanField should truncate an over-long value, got len %d", len(got))
	}
}
