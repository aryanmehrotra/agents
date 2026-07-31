package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Gate #0 is the plan's one non-negotiable rule: "No behavior is a constant in code. Every tunable is
// a config value, read at runtime, live-updatable with no code change and no redeploy." Its stated
// acceptance test is: if anyone asks "can we tune X without shipping code?" the answer is always yes.
//
// The plan also specifies HOW the gate is enforced — §1.7: "no literal constants (scanner fails the
// build)". That scanner was never written, and the gate degraded into an intention. Two literals had
// been sitting in the ranker for the life of the project as a result: a hardcoded 30-day recency
// decay constant and a hardcoded scope-specificity divisor, neither tunable, and the first silently
// disagreeing with the configurable half-life used by the forgetting layer.
//
// This is that scanner. It reads the behavioural source files and fails on any numeric literal that
// is not either (a) a default handed to cfg.F/cfg.I/cfg.Str — which IS the config mechanism — or
// (b) explicitly allowlisted below with a reason. Adding a tuning knob as a literal now fails the
// build, which is what "non-negotiable" has to mean to be worth stating.

// behaviouralFiles are the files where a numeric literal would encode POLICY. Files that are pure
// plumbing (HTTP wiring, storage, embedding transport) are excluded — a literal there is a protocol
// or buffer detail, not a tunable the org would ever want to change.
var behaviouralFiles = []string{"rank.go", "forget.go", "engine.go", "scope.go"}

// allowedLiterals are numbers that are NOT policy. Each needs a reason; the reason is the review.
var allowedLiterals = map[float64]string{
	0: "identity/zero — empty results, no-signal defaults, index origins",
	1: "identity — multiplicative unit, single-element cases, clamp ceiling",
	2: "arithmetic — squares, midpoints, halving, doubling",
	3: "arithmetic — id/prefix slicing widths",

	24:   "unit conversion — hours per day (a fact, not a policy)",
	100:  "unit conversion — percent",
	1000: "unit conversion — rounding to 3 decimal places for display",
	12:   "unit conversion — id hex width",
	60:   "unit conversion — minutes/seconds",
	10:   "display — percent rounded to 1 decimal place (×1000 then ÷10)",
	4:    "formatting — decimal places in strconv.FormatFloat",
	64:   "formatting — bit size in strconv.FormatFloat/ParseFloat",
	256:  "loop granularity — how often the scan checks its deadline; bounds clock-call overhead, not behaviour",
}

func TestGate0NoHardcodedBehaviour(t *testing.T) {
	for _, name := range behaviouralFiles {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(".", name)
			if _, err := os.Stat(path); err != nil {
				t.Skipf("%s not present", name)
			}

			fset := token.NewFileSet()

			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}

			for _, lit := range policyLiterals(file) {
				v, err := strconv.ParseFloat(lit.Value, 64)
				if err != nil {
					continue
				}

				if _, ok := allowedLiterals[v]; ok {
					continue
				}

				pos := fset.Position(lit.Pos())
				t.Errorf(
					"Gate #0: hardcoded behavioural constant %s at %s:%d — make it a config knob "+
						"(cfg.F(\"<area>.<name>\", %s)) or add it to allowedLiterals with a reason",
					lit.Value, name, pos.Line, lit.Value,
				)
			}
		})
	}
}

// policyLiterals returns every numeric literal in the file EXCEPT those in a config-default position,
// which is exactly where a tunable is supposed to live.
func policyLiterals(file *ast.File) []*ast.BasicLit {
	skip := map[ast.Node]bool{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isConfigGetter(call.Fun) || len(call.Args) < 2 {
			return true
		}

		// cfg.F("key", DEFAULT, chain...) — the default is the knob's value, not a hardcoded one.
		if lit, ok := call.Args[1].(*ast.BasicLit); ok {
			skip[lit] = true
		}

		// A negated default, e.g. cfg.F("k", -1).
		if un, ok := call.Args[1].(*ast.UnaryExpr); ok {
			if lit, ok := un.X.(*ast.BasicLit); ok {
				skip[lit] = true
			}
		}

		return true
	})

	var out []*ast.BasicLit

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || skip[lit] {
			return true
		}

		if lit.Kind == token.INT || lit.Kind == token.FLOAT {
			out = append(out, lit)
		}

		return true
	})

	return out
}

// isConfigGetter reports whether fn is a cfg.F / cfg.I / cfg.Str call. The receiver may be a plain
// identifier (`cfg.F(...)`) or a field selector (`en.cfg.F(...)`); both are the config mechanism.
func isConfigGetter(fn ast.Expr) bool {
	sel, ok := fn.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	switch sel.Sel.Name {
	case "F", "I", "Str":
	default:
		return false
	}

	return strings.Contains(strings.ToLower(exprName(sel.X)), "cfg")
}

// exprName renders the receiver of a selector as a dotted name ("cfg", "en.cfg"), or "" if it is
// some other expression shape.
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	default:
		return ""
	}
}
