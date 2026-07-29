// breaking-change-agent — a GoFr 1.58 service that detects API/contract breaking changes in a diff
// before merge. Give it the old and new content of each changed Go file and it reports exactly which
// exported symbols broke: a removed function, a changed signature, a struct field that vanished or
// changed type, an interface method added or removed. This is the review-and-release stage of the
// SDLC suite (`code-review-agent` reviews style/correctness; this agent answers one narrower, higher-
// stakes question: did this change break callers?).
//
// A model reading a diff will happily say "looks fine" about a change that silently drops an exported
// field, or invent a breakage that isn't real — LLMs are unreliable at the kind of exhaustive,
// mechanical comparison this needs. So detection is NOT delegated to the model at all: for every .go
// file, the exported API surface (top-level funcs, methods, struct fields, interface methods, typed
// vars/consts) is extracted from both versions with go/parser + go/ast and diffed deterministically in
// Go. A symbol that vanished, or whose signature/type changed, is BREAKING — full stop, regardless of
// what an LLM would have said. The model's only job is an optional one-line plain-English rationale
// attached to each already-confirmed breakage; if the model is unavailable, the verdict and the full
// list of breaking changes still stand, you just lose the prose. For non-Go input (another language, or
// a raw unified diff with no matching file pair) there is no deterministic guardrail possible, so the
// agent falls back to an LLM opinion and labels it "unverified" — it is never presented as verified fact.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxFiles     = 40
	maxFileBytes = 128 * 1024
	maxEntries   = 300
	maxRationale = 20 // cap on how many confirmed breakages get sent to the model for a rationale
)

// fileChange is one file's before/after content the caller wants compared.
type fileChange struct {
	Path string `json:"path"`
	Old  string `json:"old"` // empty means the file was added
	New  string `json:"new"` // empty means the file was deleted
}

// breakage is one confirmed (Go-verified) breaking change.
type breakage struct {
	Kind      string `json:"kind"`
	Symbol    string `json:"symbol"`
	File      string `json:"file"`
	Detail    string `json:"detail"`
	Rationale string `json:"rationale,omitempty"` // model-added, best-effort, never load-bearing
}

// fileResult is one file's analysis outcome.
type fileResult struct {
	Path      string     `json:"path"`
	Checked   bool       `json:"checked"` // true iff both sides parsed as Go and were compared
	ParseErr  string     `json:"parse_error,omitempty"`
	Breaking  []breakage `json:"breaking"`
	Additions []string   `json:"additions,omitempty"` // new/expanded exported symbols — informational, not breaking
}

func main() {
	app := gofr.New()

	app.POST("/breaking-change", breakingChange)

	app.Run()
}

func breakingChange(c *gofr.Context) (any, error) {
	var in struct {
		Title string       `json:"title"`
		Diff  string       `json:"diff"` // raw unified diff / free text — orchestrator single-string passthrough
		Text  string       `json:"text"` // alias for Diff
		Files []fileChange `json:"files"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	freeText := strings.TrimSpace(firstNonEmpty(in.Diff, in.Text))

	files, skipped := collectFiles(in.Files)
	if len(files) == 0 && freeText == "" {
		return map[string]any{
			"error": "provide `files` (an array of {path, old, new}) and/or a `diff`/`text` to analyze.",
		}, nil
	}

	results := make([]fileResult, 0, len(files))
	totalBreaking := 0

	for _, f := range files {
		fr := analyzeFile(f)
		results = append(results, fr)
		totalBreaking += len(fr.Breaking)
	}

	if totalBreaking > 0 {
		annotateRationale(c, results) // best-effort; the verdict below never depends on this succeeding
	}

	verdict := "compatible"
	if totalBreaking > 0 {
		verdict = "breaking"
	}

	out := map[string]any{
		"title":   in.Title,
		"files":   results,
		"skipped": skipped,
		"verify": map[string]any{
			"files_analyzed":   len(results),
			"breaking_changes": totalBreaking,
			"verdict":          verdict,
		},
		"note": "breaking changes are detected deterministically in Go by diffing the exported API " +
			"surface (go/ast) of old vs new for each .go file — never taken on the model's word. Files " +
			"that aren't valid Go, or that fail to parse, come back checked:false with no breakages " +
			"claimed either way.",
	}

	// Files/diff text we couldn't Go-verify (no .go pair, or unparseable) get an honest LLM-only
	// opinion instead of silence — clearly labeled as unverified, never mixed into the verified list.
	if unverified := unverifiedInputs(files, results, freeText); unverified != "" {
		out["unverified_opinion"] = unverifiedOpinion(c, in.Title, unverified)
	}

	return out, nil
}

// collectFiles validates and de-dupes the input paths, same discipline as the other agents that accept
// caller-supplied file sets (see migration-agent): cap on count and size, reject duplicates.
func collectFiles(in []fileChange) (files []fileChange, skipped []map[string]string) {
	files = []fileChange{}
	skipped = []map[string]string{}
	seen := map[string]bool{}

	for i, f := range in {
		if i >= maxEntries {
			break
		}

		p := strings.TrimSpace(f.Path)
		if p == "" {
			skipped = append(skipped, map[string]string{"path": f.Path, "reason": "empty path"})
			continue
		}

		if seen[p] {
			skipped = append(skipped, map[string]string{"path": p, "reason": "duplicate path"})
			continue
		}

		if len(f.Old) > maxFileBytes || len(f.New) > maxFileBytes {
			skipped = append(skipped, map[string]string{"path": p, "reason": "file too large"})
			continue
		}

		if len(files) >= maxFiles {
			skipped = append(skipped, map[string]string{"path": p, "reason": "exceeds file cap"})
			continue
		}

		seen[p] = true
		files = append(files, fileChange{Path: p, Old: f.Old, New: f.New})
	}

	return files, skipped
}

// analyzeFile is the guardrail's core: parse both sides of a .go file (an empty side means the file was
// added or deleted) and diff their exported API surfaces. Non-.go files, or a side that fails to parse,
// come back checked:false — no breakage is ever claimed without a successful parse on both sides.
func analyzeFile(f fileChange) fileResult {
	if !strings.HasSuffix(strings.ToLower(f.Path), ".go") {
		return fileResult{Path: f.Path, Checked: false, ParseErr: "not a .go file"}
	}

	oldAPI, err := parseAPI(f.Old)
	if err != nil {
		return fileResult{Path: f.Path, Checked: false, ParseErr: "old: " + err.Error()}
	}

	newAPI, err := parseAPI(f.New)
	if err != nil {
		return fileResult{Path: f.Path, Checked: false, ParseErr: "new: " + err.Error()}
	}

	breaking, additions := diffAPI(f.Path, oldAPI, newAPI)

	return fileResult{Path: f.Path, Checked: true, Breaking: breaking, Additions: additions}
}

// typeAPI is one exported type's shape: its kind, and — for struct/interface — its exported
// field/method set keyed by name. Fields/Methods is nil for "other" (alias / defined-over-basic) types.
type typeAPI struct {
	Kind       string // "struct", "interface", "other"
	Underlying string // only set for "other" — the underlying type expression
	Fields     map[string]string
	Methods    map[string]string
}

// api is the exported surface of one file: funcs, methods (keyed "Receiver.Method"), types, and typed
// top-level vars/consts. Untyped/unexported declarations are irrelevant to a breaking-change check.
type api struct {
	Funcs   map[string]string
	Methods map[string]string
	Types   map[string]typeAPI
	Vars    map[string]string
}

func emptyAPI() api {
	return api{Funcs: map[string]string{}, Methods: map[string]string{}, Types: map[string]typeAPI{}, Vars: map[string]string{}}
}

// parseAPI extracts the exported API surface from Go source. Empty source (a file that was added or
// deleted on this side) is a legitimate empty surface, not a parse error.
func parseAPI(src string) (api, error) {
	if strings.TrimSpace(src) == "" {
		return emptyAPI(), nil
	}

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return api{}, err
	}

	a := emptyAPI()

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			addFunc(&a, d)
		case *ast.GenDecl:
			addGenDecl(&a, d)
		}
	}

	return a, nil
}

func addFunc(a *api, d *ast.FuncDecl) {
	if !d.Name.IsExported() {
		return
	}

	sig := funcSig(d.Type)

	if d.Recv == nil || len(d.Recv.List) == 0 {
		a.Funcs[d.Name.Name] = sig
		return
	}

	recv := baseTypeName(d.Recv.List[0].Type)
	a.Methods[recv+"."+d.Name.Name] = sig
}

func addGenDecl(a *api, d *ast.GenDecl) {
	switch d.Tok {
	case token.TYPE:
		for _, spec := range d.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}

			a.Types[ts.Name.Name] = typeInfo(ts.Type)
		}
	case token.VAR, token.CONST:
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || vs.Type == nil { // only typed declarations are checkable without full type inference
				continue
			}

			t := types.ExprString(vs.Type)
			for _, n := range vs.Names {
				if n.IsExported() {
					a.Vars[n.Name] = t
				}
			}
		}
	}
}

// typeInfo classifies a type declaration's underlying expression into a comparable shape.
func typeInfo(expr ast.Expr) typeAPI {
	switch t := expr.(type) {
	case *ast.StructType:
		fields := map[string]string{}

		for _, f := range t.Fields.List {
			ft := types.ExprString(f.Type)

			if len(f.Names) == 0 { // embedded field — the field name IS the (base) type name
				name := baseTypeName(f.Type)
				if ast.IsExported(name) {
					fields[name] = ft
				}

				continue
			}

			for _, n := range f.Names {
				if n.IsExported() {
					fields[n.Name] = ft
				}
			}
		}

		return typeAPI{Kind: "struct", Fields: fields}

	case *ast.InterfaceType:
		methods := map[string]string{}

		for _, m := range t.Methods.List {
			if len(m.Names) == 0 { // embedded interface
				name := baseTypeName(m.Type)
				if ast.IsExported(name) {
					methods["~"+name] = types.ExprString(m.Type) // "~" marks an embed, not a method
				}

				continue
			}

			ft, ok := m.Type.(*ast.FuncType)
			if !ok {
				continue
			}

			for _, n := range m.Names {
				if n.IsExported() {
					methods[n.Name] = funcSig(ft)
				}
			}
		}

		return typeAPI{Kind: "interface", Methods: methods}

	default:
		return typeAPI{Kind: "other", Underlying: types.ExprString(expr)}
	}
}

// funcSig builds a normalized, comparable signature string from a function type: parameter types in
// order (one entry per name, so `a, b int` counts as two params) then result types, names dropped.
func funcSig(ft *ast.FuncType) string {
	return "(" + strings.Join(fieldTypes(ft.Params), ", ") + ") (" + strings.Join(fieldTypes(ft.Results), ", ") + ")"
}

func fieldTypes(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}

	out := []string{}

	for _, f := range fl.List {
		t := types.ExprString(f.Type)
		n := len(f.Names)

		if n == 0 {
			n = 1
		}

		for i := 0; i < n; i++ {
			out = append(out, t)
		}
	}

	return out
}

// baseTypeName strips pointers/generics off a type expression to get the identifier a receiver or
// embed refers to, e.g. *Foo, Foo[T], *Foo[T] all yield "Foo".
func baseTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return baseTypeName(t.X)
	case *ast.IndexExpr:
		return baseTypeName(t.X)
	case *ast.IndexListExpr:
		return baseTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	default:
		return types.ExprString(expr)
	}
}

// diffAPI is the guardrail's verdict: it walks every exported symbol in old and new and classifies each
// difference as breaking or a plain addition. Nothing here is model output — it is a pure comparison of
// two parsed ASTs.
func diffAPI(path string, oldAPI, newAPI api) (breaking []breakage, additions []string) {
	breaking = []breakage{}
	additions = []string{}

	// top-level funcs
	for name, oldSig := range oldAPI.Funcs {
		newSig, ok := newAPI.Funcs[name]
		if !ok {
			breaking = append(breaking, mk(path, "removed-func", name, "exported func "+name+" was removed"))
			continue
		}

		if newSig != oldSig {
			breaking = append(breaking, mk(path, "signature-changed", name,
				fmt.Sprintf("func %s signature changed from %s to %s", name, oldSig, newSig)))
		}
	}

	for name := range newAPI.Funcs {
		if _, ok := oldAPI.Funcs[name]; !ok {
			additions = append(additions, "func "+name)
		}
	}

	// methods
	for name, oldSig := range oldAPI.Methods {
		newSig, ok := newAPI.Methods[name]
		if !ok {
			breaking = append(breaking, mk(path, "removed-method", name, "exported method "+name+" was removed"))
			continue
		}

		if newSig != oldSig {
			breaking = append(breaking, mk(path, "method-signature-changed", name,
				fmt.Sprintf("method %s signature changed from %s to %s", name, oldSig, newSig)))
		}
	}

	for name := range newAPI.Methods {
		if _, ok := oldAPI.Methods[name]; !ok {
			additions = append(additions, "method "+name)
		}
	}

	// types (and their fields/interface methods)
	for name, oldT := range oldAPI.Types {
		newT, ok := newAPI.Types[name]
		if !ok {
			breaking = append(breaking, mk(path, "removed-type", name, "exported type "+name+" was removed"))
			continue
		}

		breaking = append(breaking, diffType(path, name, oldT, newT)...)

		for f := range newT.Fields {
			if _, ok := oldT.Fields[f]; !ok {
				additions = append(additions, "field "+name+"."+f)
			}
		}
	}

	for name := range newAPI.Types {
		if _, ok := oldAPI.Types[name]; !ok {
			additions = append(additions, "type "+name)
		}
	}

	// typed top-level vars/consts
	for name, oldType := range oldAPI.Vars {
		newType, ok := newAPI.Vars[name]
		if !ok {
			breaking = append(breaking, mk(path, "removed-var", name, "exported var/const "+name+" was removed"))
			continue
		}

		if newType != oldType {
			breaking = append(breaking, mk(path, "var-type-changed", name,
				fmt.Sprintf("%s changed type from %s to %s", name, oldType, newType)))
		}
	}

	for name := range newAPI.Vars {
		if _, ok := oldAPI.Vars[name]; !ok {
			additions = append(additions, "var "+name)
		}
	}

	sort.Slice(breaking, func(i, j int) bool { return breaking[i].Symbol < breaking[j].Symbol })
	sort.Strings(additions)

	return breaking, additions
}

// diffType compares one type's shape across versions. A struct losing/retyping an exported field is
// breaking; a struct gaining one is not. An interface is the opposite in one direction: gaining a
// method breaks anyone implementing it, so BOTH directions are breaking for interfaces.
func diffType(path, name string, oldT, newT typeAPI) []breakage {
	out := []breakage{}

	if oldT.Kind != newT.Kind {
		out = append(out, mk(path, "type-kind-changed", name,
			fmt.Sprintf("%s changed from a %s to a %s", name, oldT.Kind, newT.Kind)))
		return out // shape is incomparable beyond this point
	}

	switch oldT.Kind {
	case "other":
		if oldT.Underlying != newT.Underlying {
			out = append(out, mk(path, "type-changed", name,
				fmt.Sprintf("%s underlying type changed from %s to %s", name, oldT.Underlying, newT.Underlying)))
		}

	case "struct":
		for f, oldFT := range oldT.Fields {
			newFT, ok := newT.Fields[f]
			if !ok {
				out = append(out, mk(path, "field-removed", name+"."+f, "exported field "+f+" removed from "+name))
				continue
			}

			if newFT != oldFT {
				out = append(out, mk(path, "field-type-changed", name+"."+f,
					fmt.Sprintf("field %s.%s changed type from %s to %s", name, f, oldFT, newFT)))
			}
		}

	case "interface":
		for m, oldSig := range oldT.Methods {
			newSig, ok := newT.Methods[m]
			if !ok {
				out = append(out, mk(path, "interface-method-removed", name+"."+strings.TrimPrefix(m, "~"),
					"interface "+name+" lost method/embed "+strings.TrimPrefix(m, "~")+" — breaks callers"))
				continue
			}

			if newSig != oldSig {
				out = append(out, mk(path, "interface-method-signature-changed", name+"."+m,
					fmt.Sprintf("interface %s method %s signature changed from %s to %s", name, m, oldSig, newSig)))
			}
		}

		for m := range newT.Methods {
			if _, ok := oldT.Methods[m]; !ok {
				out = append(out, mk(path, "interface-method-added", name+"."+strings.TrimPrefix(m, "~"),
					"interface "+name+" gained method/embed "+strings.TrimPrefix(m, "~")+
						" — breaks existing implementers"))
			}
		}
	}

	return out
}

func mk(file, kind, symbol, detail string) breakage {
	return breakage{Kind: kind, Symbol: symbol, File: file, Detail: detail}
}

// annotateRationale asks the model for a one-line plain-English rationale per confirmed breakage. It is
// pure decoration: matching is best-effort by (file, symbol, kind), a model outage or a malformed reply
// leaves every Rationale field simply empty, and the breaking-change list itself is untouched.
func annotateRationale(c *gofr.Context, results []fileResult) {
	items := make([]breakage, 0, maxRationale)

	for i := range results {
		for j := range results[i].Breaking {
			if len(items) >= maxRationale {
				break
			}

			items = append(items, results[i].Breaking[j])
		}
	}

	if len(items) == 0 {
		return
	}

	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- [%s] %s in %s: %s\n", it.Kind, it.Symbol, it.File, it.Detail)
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "Each line below is an ALREADY-CONFIRMED API breaking change " +
			"(confirmed by static analysis, not by you). For each, write ONE short plain-English sentence " +
			"explaining the impact on callers. Reply with ONLY a JSON array, same order as the input: " +
			"[{\"symbol\":\"...\",\"rationale\":\"...\"}]. Do not add or drop items, and do not second-guess " +
			"whether something is actually breaking — that was already decided."},
		{Role: ai.RoleUser, Content: b.String()},
	}, ai.WithTemperature(0.1))
	if err != nil {
		return // degrade silently — the confirmed list stands without prose
	}

	rationales := extractRationales(resp.Content)

	for i := range results {
		for j := range results[i].Breaking {
			if r, ok := rationales[results[i].Breaking[j].Symbol]; ok {
				results[i].Breaking[j].Rationale = r
			}
		}
	}
}

// unverifiedInputs returns the free text (if any) worth an LLM-only opinion: an explicit `diff`/`text`
// caller input, or the content of files that couldn't be Go-verified (non-.go, or failed to parse).
func unverifiedInputs(files []fileChange, results []fileResult, freeText string) string {
	var b strings.Builder

	if freeText != "" {
		b.WriteString(freeText)
		b.WriteByte('\n')
	}

	byPath := map[string]fileChange{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	for _, r := range results {
		if r.Checked {
			continue
		}

		f := byPath[r.Path]
		fmt.Fprintf(&b, "\n--- %s (old) ---\n%s\n--- %s (new) ---\n%s\n", f.Path, f.Old, f.Path, f.New)
	}

	return strings.TrimSpace(b.String())
}

// unverifiedOpinion is the honest fallback for input the Go guardrail can't check: an LLM's best guess,
// clearly labeled as such — never merged into the verified `files`/`verify` result above.
func unverifiedOpinion(c *gofr.Context, title, text string) map[string]any {
	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are reviewing a change for API/contract breaking changes in " +
			"input that isn't Go source we can statically verify (another language, or a raw diff). Give " +
			"your best assessment: what looks like it might break callers, and how confident you are. Be " +
			"concise."},
		{Role: ai.RoleUser, Content: "Title: " + title + "\n\n" + text},
	}, ai.WithTemperature(0.2))
	if err != nil {
		return map[string]any{
			"verified": false,
			"error":    "model unavailable, no opinion could be generated: " + err.Error(),
		}
	}

	return map[string]any{
		"verified": false,
		"opinion":  resp.Content,
		"caveat":   "not Go-verified — this is an LLM opinion only, with no deterministic guardrail behind it",
	}
}

// extractRationales pulls a best-effort symbol->rationale map out of the model's reply. Any parse
// failure just yields an empty map — annotateRationale already tolerates that.
func extractRationales(s string) map[string]string {
	out := map[string]string{}

	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')

	if start < 0 || end <= start {
		return out
	}

	type entry struct {
		Symbol    string `json:"symbol"`
		Rationale string `json:"rationale"`
	}

	var entries []entry
	if err := json.Unmarshal([]byte(s[start:end+1]), &entries); err != nil {
		return out
	}

	for _, e := range entries {
		if e.Symbol != "" && e.Rationale != "" {
			out[e.Symbol] = e.Rationale
		}
	}

	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
