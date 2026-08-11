// dependency-agent — a GoFr 1.58 service that proposes and validates dependency bumps for a Go
// module. Give it a go.mod and a list of available updates (e.g. what `go list -m -u all` reports)
// and it tells you which ones are safe to take now, applying only those to the go.mod text. This is
// the "review & release" stage of the software-development lifecycle suite — the pattern behind this
// repo's own daily dependency PRs.
//
// The model only judges: given the available updates and their release notes, it decides which ones
// are worth taking now vs holding — that's a genuine judgement call (does this patch fix a CVE? does
// this changelog mention a breaking behaviour change?). Everything else is decided deterministically
// in Go, because a model asked about dependency bumps will happily invent a package that was never
// offered, misjudge a major version as safe, or "fix" a go.mod line into something that no longer
// parses:
//
//   - the ONLY modules ever considered are the ones the caller listed in `updates` — a model can
//     annotate an entry that isn't there, but there is no code path that turns that into a bump;
//   - the semver bump kind (patch/minor/major) is computed from the version numbers themselves, never
//     taken on the model's or the caller's word;
//   - a MAJOR version bump is never auto-applied — Go overrides the model's judgement and always holds
//     it for manual review, because a major bump can break the import path or the API entirely;
//   - a bump is only written into the go.mod text if the module's CURRENT version there exactly
//     matches what the caller claimed — otherwise the caller's picture of the module is already stale
//     and the edit is rejected rather than applied blind;
//   - after the edit, the rewritten require line is re-checked against the go.mod require-line
//     grammar; if that ever fails the original line is kept — a bump must never leave a go.mod broken.
//
// It's all in-process (no disk writes, no `go get`, no network) — you get back the verdicts and the
// rewritten go.mod text to apply and build-gate yourself.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxUpdates     = 200
	maxGoModBytes  = 256 * 1024
	maxNotesChars  = 500
	decisionApply  = "apply"
	decisionHold   = "hold_for_review"
	decisionReject = "rejected"
)

var semverRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)`)

// update is one available dependency bump the caller is offering, e.g. one row of `go list -m -u all`
// plus whatever release-note text the caller has for it. This is the ONLY source of truth for what
// modules exist to bump — the model is never asked to enumerate them.
type update struct {
	Module  string `json:"module"`
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Notes   string `json:"notes"` // optional changelog/release-note snippet
}

// verdict is the per-module outcome: the deterministic classification plus (only when Go allowed it
// to be judged) the model's advisory rationale.
type verdict struct {
	Module      string `json:"module"`
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	Notes       string `json:"-"`                   // caller's release notes, only used to build the prompt
	Bump        string `json:"bump"`                // patch | minor | major | invalid — computed in Go
	Decision    string `json:"decision"`            // apply | hold_for_review | rejected
	Reason      string `json:"reason,omitempty"`    // why Go decided/overrode
	Rationale   string `json:"rationale,omitempty"` // model's advisory judgement (only for eligible modules)
	Applied     bool   `json:"applied"`
	AppliedDiff string `json:"applied_diff,omitempty"`
}

func main() {
	app := gofr.New()

	app.POST("/dependencies", dependencies)

	app.Run()
}

func dependencies(c *gofr.Context) (any, error) {
	var in struct {
		GoMod   string   `json:"go_mod"`
		Text    string   `json:"text"` // alias slot for the orchestrator's single-string call
		Updates []update `json:"updates"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	goMod := in.GoMod
	if len(in.Updates) == 0 || strings.TrimSpace(goMod) == "" {
		return map[string]any{
			"error": "provide `go_mod` (the module's go.mod text) and `updates` (an array of " +
				"{module, current, latest, notes?} — e.g. from `go list -m -u all`). Only modules " +
				"listed in `updates` are ever considered for a bump; nothing else is invented.",
		}, nil
	}

	if len(goMod) > maxGoModBytes {
		return map[string]any{"error": "go_mod too large"}, nil
	}

	if len(in.Updates) > maxUpdates {
		in.Updates = in.Updates[:maxUpdates]
	}

	verdicts := classify(in.Updates)

	// Advisory only: for the modules Go already deemed eligible (valid semver, genuinely newer, not a
	// major bump), ask the model whether to take the update now or hold, given the release notes. A
	// model outage just leaves those held for manual review — the classification above doesn't change.
	annotate(c, verdicts)

	updatedGoMod, applied, held, rejected := applyBumps(goMod, verdicts)

	return map[string]any{
		"summary": map[string]any{
			"considered": len(verdicts),
			"applied":    applied,
			"held":       held,
			"rejected":   rejected,
		},
		"verdicts":       verdicts,
		"updated_go_mod": updatedGoMod,
		"note": "only modules present in the caller-supplied `updates` list are ever touched. Major " +
			"version bumps always hold for manual review regardless of the model's judgement. A bump " +
			"is written only if the module's current version in go_mod matches what was claimed, and " +
			"the rewritten require line is re-checked before being kept.",
	}, nil
}

// classify is the deterministic core: for every caller-supplied update it computes the real semver
// bump kind from the version numbers and applies the guardrail policy — invalid/non-newer versions are
// rejected outright, and a major bump always holds for manual review. Everything else is left
// "pending" for the model's advisory judgement in annotate.
func classify(updates []update) []verdict {
	verdicts := make([]verdict, 0, len(updates))
	seen := map[string]bool{}

	for _, u := range updates {
		mod := strings.TrimSpace(u.Module)
		if mod == "" || seen[mod] {
			continue // ignore blank/duplicate entries rather than silently overwrite one
		}
		seen[mod] = true

		v := verdict{
			Module:  mod,
			Current: strings.TrimSpace(u.Current),
			Latest:  strings.TrimSpace(u.Latest),
			Notes:   cleanNotes(u.Notes),
		}

		curMaj, curMin, curPatch, curOK := parseSemver(v.Current)
		latMaj, latMin, latPatch, latOK := parseSemver(v.Latest)

		switch {
		case !curOK || !latOK:
			v.Bump = "invalid"
			v.Decision = decisionReject
			v.Reason = "current/latest is not a valid vX.Y.Z semver version"
		case !isNewer(curMaj, curMin, curPatch, latMaj, latMin, latPatch):
			v.Bump = "invalid"
			v.Decision = decisionReject
			v.Reason = "latest is not newer than current"
		case latMaj != curMaj:
			v.Bump = "major"
			v.Decision = decisionHold
			v.Reason = "major version bump — always held for manual review, never auto-applied"
		case latMin != curMin:
			v.Bump = "minor"
			v.Decision = "pending" // awaiting model judgement in annotate
		default:
			v.Bump = "patch"
			v.Decision = "pending"
		}

		verdicts = append(verdicts, v)
	}

	return verdicts
}

// parseSemver extracts the major/minor/patch integers from a "vX.Y.Z..." string (a pre-release or
// build suffix after the patch number is allowed and ignored). Returns ok=false for anything else.
func parseSemver(s string) (major, minor, patch int, ok bool) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, 0, false
	}

	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])

	return major, minor, patch, true
}

func isNewer(curMaj, curMin, curPatch, latMaj, latMin, latPatch int) bool {
	if latMaj != curMaj {
		return latMaj > curMaj
	}

	if latMin != curMin {
		return latMin > curMin
	}

	return latPatch > curPatch
}

// annotate asks the model which of the "pending" (already Go-vetted: valid semver, genuinely newer,
// not major) updates to apply now vs hold, given their release notes. The guardrail here is structural
// rather than a check to fail: the model can only set apply/hold_for_review on entries that already
// exist in `verdicts` (by exact module name) — there is no code path through which its reply could
// introduce a module that wasn't in the caller's `updates` list in the first place. Any decision word
// other than "apply" is treated as a hold (fail safe).
func annotate(c *gofr.Context, verdicts []verdict) {
	prompt, pending := pendingPrompt(verdicts)
	if prompt == "" {
		return
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a dependency-update reviewer for a Go project. For each " +
			"listed module bump (already confirmed to be a safe patch/minor semver bump — never a major " +
			"one), decide \"apply\" if it looks safe to take now, or \"hold_for_review\" if the notes " +
			"suggest risk (breaking behaviour, deprecations, anything that needs a human look) or you're " +
			"unsure. Reply with ONLY a JSON array of {\"module\": string, \"decision\": \"apply\"|" +
			"\"hold_for_review\", \"rationale\": string}. Only judge modules you were given — do not add " +
			"any others."},
		{Role: ai.RoleUser, Content: prompt},
	}, ai.WithTemperature(0))
	if err != nil {
		c.Logger.Errorf("dependency-agent: judgement model unavailable, holding pending updates for manual review: %v", err)
		holdAllPending(verdicts, "model unavailable — held for manual review by default")

		return
	}

	var ann []annotation
	if perr := json.Unmarshal([]byte(extractArray(resp.Content)), &ann); perr != nil {
		holdAllPending(verdicts, "model reply was not valid JSON — held for manual review by default")
		return
	}

	mergeAnnotations(verdicts, pending, ann)
}

// annotation is the model's per-module reply shape.
type annotation struct {
	Module    string `json:"module"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale"`
}

// pendingPrompt renders the prompt listing every still-"pending" verdict (already Go-vetted: valid
// semver, genuinely newer, not major), and returns the module→verdicts-index map the reply is merged
// back against. Returns an empty prompt when there's nothing pending.
func pendingPrompt(verdicts []verdict) (prompt string, pending map[string]int) {
	pending = map[string]int{}

	var b strings.Builder

	for i, v := range verdicts {
		if v.Decision != "pending" {
			continue
		}

		pending[v.Module] = i

		suffix := ""
		if v.Notes != "" {
			suffix = ": " + v.Notes
		}

		fmt.Fprintf(&b, "- %s: %s -> %s (%s bump)%s\n", v.Module, v.Current, v.Latest, v.Bump, suffix)
	}

	if len(pending) == 0 {
		return "", pending
	}

	return b.String(), pending
}

// mergeAnnotations is the guardrail: it applies the model's apply/hold_for_review decisions onto the
// verdicts the caller actually offered (via `pending`, built from the SAME `updates` list classify
// worked from) and nothing else. A reply naming a module that isn't in `pending` — say a model that
// was prompt-injected into inventing an unrelated package — has no entry to write into and is simply
// dropped; there is no path from an unknown module name to a verdict. A decision word other than
// "apply" (or a module the model never answered for) resolves to hold_for_review, never to apply.
func mergeAnnotations(verdicts []verdict, pending map[string]int, ann []annotation) {
	handled := map[string]bool{}

	for _, a := range ann {
		mod := strings.TrimSpace(a.Module)

		i, known := pending[mod]
		if !known {
			continue // named a module we never offered it — ignored, not applied
		}

		handled[mod] = true

		v := &verdicts[i]
		v.Rationale = strings.TrimSpace(a.Rationale)

		if strings.TrimSpace(strings.ToLower(a.Decision)) == decisionApply {
			v.Decision = decisionApply
		} else {
			v.Decision = decisionHold
			if v.Reason == "" {
				v.Reason = "model judged this update should be reviewed manually"
			}
		}
	}

	// Anything still pending (the model skipped it) is held, never silently applied.
	for mod, i := range pending {
		if !handled[mod] {
			verdicts[i].Decision = decisionHold
			verdicts[i].Reason = "model did not return a judgement — held for manual review by default"
		}
	}
}

func cleanNotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxNotesChars {
		s = s[:maxNotesChars] + "…"
	}

	return s
}

func holdAllPending(verdicts []verdict, reason string) {
	for i := range verdicts {
		if verdicts[i].Decision == "pending" {
			verdicts[i].Decision = decisionHold
			verdicts[i].Reason = reason
		}
	}
}

// applyBumps is the second half of the guardrail: it writes only the "apply" verdicts into the go.mod
// text, and only if the module's CURRENT version there matches what the caller claimed — a stale
// picture of the module is rejected rather than applied blind. Every edit is re-checked against the
// require-line grammar before being kept.
func applyBumps(goMod string, verdicts []verdict) (updated string, applied, held, rejected int) {
	updated = goMod

	for i := range verdicts {
		v := &verdicts[i]

		switch v.Decision {
		case decisionHold:
			held++
			continue
		case decisionReject:
			rejected++
			continue
		case decisionApply:
			// fall through to the edit below
		default:
			held++ // defensive: any state we don't recognise is never applied
			v.Decision = decisionHold
			continue
		}

		newText, diff, ok := replaceRequireVersion(updated, v.Module, v.Current, v.Latest)
		if !ok {
			v.Decision = decisionReject
			v.Reason = "current version not found (exactly once) in go_mod — the supplied go.mod is " +
				"stale relative to the claimed current version, so nothing was written"
			rejected++

			continue
		}

		updated = newText
		v.Applied = true
		v.AppliedDiff = diff
		applied++
	}

	return updated, applied, held, rejected
}

// requireLinePattern matches a `require` line for the given module at the given exact version: the
// module path, whitespace, the version, then whitespace/comment/end-of-line — so "v1.2.0" can never
// accidentally match a line actually pinned to "v1.2.0-beta" or "v1.2.00".
func requireLinePattern(module, version string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^([ \t]*` + regexp.QuoteMeta(module) + `[ \t]+)` +
		regexp.QuoteMeta(version) + `([ \t]|$)`)
}

// replaceRequireVersion rewrites the single require line for module@currentVersion to newVersion.
// It requires EXACTLY one match — zero (stale/missing) or more than one (ambiguous) both refuse the
// edit — and re-checks the rewritten line still parses as a valid require line before keeping it.
func replaceRequireVersion(goMod, module, currentVersion, newVersion string) (updated, diff string, ok bool) {
	pattern := requireLinePattern(module, currentVersion)

	if len(pattern.FindAllStringIndex(goMod, 2)) != 1 {
		return goMod, "", false
	}

	// In a ReplaceAllString template, only "$" is special (it starts a "$1"/"${1}" backreference) — so
	// the version text needs "$" doubled to "$$", not full regexp.QuoteMeta (which would also escape
	// ordinary characters like "." and corrupt an ordinary version string such as "v1.58.0").
	updated = pattern.ReplaceAllString(goMod, `${1}`+strings.ReplaceAll(newVersion, "$", "$$")+`${2}`)

	if !requireLineValid(updated, module, newVersion) {
		return goMod, "", false // never keep an edit that doesn't verify — original is returned
	}

	return updated, fmt.Sprintf("- %s %s\n+ %s %s", module, currentVersion, module, newVersion), true
}

// requireLineValid re-verifies the rewritten go.mod: the module must now appear at exactly the new
// version, and the old version must be gone. This is the "still parses" check, adapted to the
// require-line grammar rather than a full go.mod parse.
func requireLineValid(goMod, module, newVersion string) bool {
	return len(requireLinePattern(module, newVersion).FindAllStringIndex(goMod, 2)) == 1
}

// extractArray pulls the first balanced JSON array out of a model response that may wrap it in fences
// or prose; returns "[]" when there's none, so json.Unmarshal simply yields no annotations.
func extractArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return "[]"
	}

	depth := 0
	inStr := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]

		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inStr:
			escaped = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '[':
			depth++
		case ch == ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}

	return "[]"
}
