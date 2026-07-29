package main

import "testing"

// TestDiffAPIFuncRemoved is the core guarantee: an exported func that vanishes is breaking, regardless
// of what any model would say about it.
func TestDiffAPIFuncRemoved(t *testing.T) {
	oldAPI := emptyAPI()
	oldAPI.Funcs["Foo"] = "(int) (string)"

	newAPI := emptyAPI()

	breaking, additions := diffAPI("a.go", oldAPI, newAPI)

	if len(breaking) != 1 || breaking[0].Kind != "removed-func" || breaking[0].Symbol != "Foo" {
		t.Fatalf("breaking = %#v, want one removed-func Foo", breaking)
	}

	if len(additions) != 0 {
		t.Errorf("additions = %v, want none", additions)
	}
}

// TestDiffAPIFuncSignatureChanged catches a parameter added to an existing exported func.
func TestDiffAPIFuncSignatureChanged(t *testing.T) {
	oldAPI := emptyAPI()
	oldAPI.Funcs["Foo"] = "(int) (string)"

	newAPI := emptyAPI()
	newAPI.Funcs["Foo"] = "(int, int) (string)"

	breaking, _ := diffAPI("a.go", oldAPI, newAPI)

	if len(breaking) != 1 || breaking[0].Kind != "signature-changed" {
		t.Fatalf("breaking = %#v, want one signature-changed", breaking)
	}
}

// TestDiffAPIFuncAdditionIsNotBreaking: a brand new exported func is a plain addition, never breaking.
func TestDiffAPIFuncAdditionIsNotBreaking(t *testing.T) {
	oldAPI := emptyAPI()
	newAPI := emptyAPI()
	newAPI.Funcs["Bar"] = "() ()"

	breaking, additions := diffAPI("a.go", oldAPI, newAPI)

	if len(breaking) != 0 {
		t.Errorf("breaking = %#v, want none", breaking)
	}

	if len(additions) != 1 || additions[0] != "func Bar" {
		t.Errorf("additions = %v, want [func Bar]", additions)
	}
}

// TestDiffAPIStructField covers the three struct-field cases: removed and retyped are breaking, added
// is not.
func TestDiffAPIStructField(t *testing.T) {
	oldAPI := emptyAPI()
	oldAPI.Types["User"] = typeAPI{Kind: "struct", Fields: map[string]string{
		"Name": "string",
		"Age":  "int",
	}}

	newAPI := emptyAPI()
	newAPI.Types["User"] = typeAPI{Kind: "struct", Fields: map[string]string{
		"Age":   "string", // retyped: int -> string
		"Email": "string", // added
		// Name removed
	}}

	breaking, additions := diffAPI("a.go", oldAPI, newAPI)

	kinds := map[string]string{}
	for _, b := range breaking {
		kinds[b.Symbol] = b.Kind
	}

	if kinds["User.Name"] != "field-removed" {
		t.Errorf("User.Name kind = %q, want field-removed", kinds["User.Name"])
	}

	if kinds["User.Age"] != "field-type-changed" {
		t.Errorf("User.Age kind = %q, want field-type-changed", kinds["User.Age"])
	}

	if len(breaking) != 2 {
		t.Errorf("breaking = %#v, want exactly 2 entries", breaking)
	}

	if len(additions) != 1 || additions[0] != "field User.Email" {
		t.Errorf("additions = %v, want [field User.Email]", additions)
	}
}

// TestDiffAPIInterfaceBothDirectionsBreak is the subtle case: unlike a struct, adding a method to an
// exported interface is ALSO breaking (it breaks anyone implementing the interface), not just removing
// one.
func TestDiffAPIInterfaceBothDirectionsBreak(t *testing.T) {
	oldAPI := emptyAPI()
	oldAPI.Types["Store"] = typeAPI{Kind: "interface", Methods: map[string]string{
		"Get": "(string) (string, error)",
	}}

	newAPI := emptyAPI()
	newAPI.Types["Store"] = typeAPI{Kind: "interface", Methods: map[string]string{
		"Set": "(string, string) (error)", // Get removed, Set added
	}}

	breaking, _ := diffAPI("a.go", oldAPI, newAPI)

	kinds := map[string]bool{}
	for _, b := range breaking {
		kinds[b.Kind] = true
	}

	if !kinds["interface-method-removed"] {
		t.Error("removing an interface method should be breaking")
	}

	if !kinds["interface-method-added"] {
		t.Error("adding an interface method should ALSO be breaking (breaks implementers)")
	}
}

// TestDiffAPITypeRemovedAndKindChanged covers a whole exported type disappearing, and a type changing
// shape (e.g. struct -> interface) being flagged even though field-level comparison no longer applies.
func TestDiffAPITypeRemovedAndKindChanged(t *testing.T) {
	oldAPI := emptyAPI()
	oldAPI.Types["Gone"] = typeAPI{Kind: "struct", Fields: map[string]string{"X": "int"}}
	oldAPI.Types["Shifted"] = typeAPI{Kind: "struct", Fields: map[string]string{}}

	newAPI := emptyAPI()
	newAPI.Types["Shifted"] = typeAPI{Kind: "interface", Methods: map[string]string{}}

	breaking, _ := diffAPI("a.go", oldAPI, newAPI)

	var sawRemoved, sawKindChanged bool

	for _, b := range breaking {
		switch {
		case b.Kind == "removed-type" && b.Symbol == "Gone":
			sawRemoved = true
		case b.Kind == "type-kind-changed" && b.Symbol == "Shifted":
			sawKindChanged = true
		}
	}

	if !sawRemoved {
		t.Error("removing an exported type should be breaking")
	}

	if !sawKindChanged {
		t.Error("struct -> interface should be flagged as a kind change")
	}
}

// TestAnalyzeFileAddedAndDeleted: an added file (empty old) has no old surface to break, so everything
// is an addition; a deleted file (empty new) loses its whole surface, so everything is breaking.
func TestAnalyzeFileAddedAndDeleted(t *testing.T) {
	added := analyzeFile(fileChange{Path: "new.go", Old: "", New: "package p\n\nfunc Foo() {}\n"})
	if !added.Checked || len(added.Breaking) != 0 || len(added.Additions) != 1 {
		t.Fatalf("added file result = %#v", added)
	}

	deleted := analyzeFile(fileChange{Path: "old.go", Old: "package p\n\nfunc Foo() {}\n", New: ""})
	if !deleted.Checked || len(deleted.Breaking) != 1 || deleted.Breaking[0].Kind != "removed-func" {
		t.Fatalf("deleted file result = %#v", deleted)
	}
}

// TestAnalyzeFileParseErrorClaimsNoBreakage is the guardrail's safety net: if either side fails to
// parse, the file comes back checked:false with NO breakage claimed — we never guess.
func TestAnalyzeFileParseErrorClaimsNoBreakage(t *testing.T) {
	r := analyzeFile(fileChange{
		Path: "broken.go",
		Old:  "package p\n\nfunc Foo() {}\n",
		New:  "package p\n\nfunc Foo( {\n", // syntax error
	})

	if r.Checked {
		t.Error("a file that fails to parse must not be marked checked")
	}

	if len(r.Breaking) != 0 {
		t.Errorf("breaking = %#v, want none claimed on a parse failure", r.Breaking)
	}

	if r.ParseErr == "" {
		t.Error("expected a non-empty parse error")
	}
}

// TestAnalyzeFileNonGoSkipped: files outside the guardrail's language are never silently blessed.
func TestAnalyzeFileNonGoSkipped(t *testing.T) {
	r := analyzeFile(fileChange{Path: "script.py", Old: "def f(): pass", New: "def f(x): pass"})
	if r.Checked {
		t.Error("a non-.go file must not be marked checked")
	}
}

// TestCollectFilesDedupesAndCaps mirrors migration-agent's input hygiene: duplicates and oversized
// entries are skipped, not silently merged or truncated into something else.
func TestCollectFilesDedupesAndCaps(t *testing.T) {
	files, skipped := collectFiles([]fileChange{
		{Path: "a.go", Old: "x", New: "y"},
		{Path: "a.go", Old: "dup", New: "dup"},
		{Path: "", Old: "z", New: "z"},
	})

	if len(files) != 1 || files[0].Path != "a.go" || files[0].Old != "x" {
		t.Fatalf("files = %#v, want just the first a.go entry kept", files)
	}

	if len(skipped) != 2 {
		t.Fatalf("skipped = %#v, want 2 entries (dup + empty path)", skipped)
	}
}

// TestExtractRationalesTolerantOfNoise: a malformed or fenced reply degrades to an empty map rather
// than panicking or poisoning the confirmed breaking-change list.
func TestExtractRationalesTolerantOfNoise(t *testing.T) {
	got := extractRationales("Sure, here you go:\n```json\n[{\"symbol\":\"Foo\",\"rationale\":\"callers break\"}]\n```")
	if got["Foo"] != "callers break" {
		t.Errorf("extractRationales = %v, want Foo mapped", got)
	}

	if len(extractRationales("no json here")) != 0 {
		t.Error("extractRationales of noise should be empty, not panic")
	}
}
