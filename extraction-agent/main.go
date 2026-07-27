// extraction-agent — a GoFr 1.58 service that turns unstructured text into structured, typed JSON
// against a caller-declared schema. "Structured output" / document-and-text extraction — pulling
// invoice fields, resume attributes, contract terms or entities out of free-form text into a strict
// schema — is the single most widely deployed LLM pattern going into production right now, the
// backbone of "chat with your documents", form auto-fill and unstructured-to-structured ETL.
//
// A language model is good at *finding* the values but cannot be trusted to *shape* them: it invents
// keys, returns the wrong type, or wraps its JSON in prose. So the model only proposes — every value
// it returns is validated deterministically in Go against the requested schema before it leaves the
// service: keys that weren't asked for are dropped, values that don't match their declared type are
// rejected, and required fields that never resolved are reported as missing. The caller gets typed
// data it can rely on, never the model's raw guess.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const maxTextChars = 20000

// scalarTypes are the value types a field can declare. A field type may also be a list of any of
// these, written "<type>[]" (e.g. "string[]"), handled uniformly by coerce.
var scalarTypes = map[string]bool{
	"string": true, "integer": true, "number": true, "boolean": true, "date": true,
}

// dateLayouts are accepted for a "date" field; whatever matches is normalized to YYYY-MM-DD, so the
// caller always gets one date shape back regardless of how the source text (or the model) wrote it.
var dateLayouts = []string{"2006-01-02", time.RFC3339, "2006/01/02", "01/02/2006", "January 2, 2006", "2 Jan 2006"}

// fieldSpec is one entry in the schema the caller wants extracted. Type is a scalar type or a
// "<scalar>[]" list; Required drives the "missing" report; Description is passed to the model to
// disambiguate what the field means.
type fieldSpec struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// defaultSchema is used when the caller sends text with no schema — an "open extraction" of the
// entities most texts carry. It keeps the agent routable from the orchestrator (which only forwards
// a single query string) while still exercising the same deterministic type validation.
var defaultSchema = []fieldSpec{
	{Name: "summary", Type: "string", Description: "one-sentence summary of what this text is about"},
	{Name: "people", Type: "string[]", Description: "full names of people mentioned"},
	{Name: "organizations", Type: "string[]", Description: "companies or organizations mentioned"},
	{Name: "emails", Type: "string[]", Description: "email addresses that appear in the text"},
	{Name: "dates", Type: "string[]", Description: "dates mentioned, exactly as written"},
	{Name: "amounts", Type: "number[]", Description: "monetary amounts or other key numbers"},
}

// rejection reports a value the model returned for a requested field that failed type validation —
// surfaced so a caller can see the model tried but produced an unusable value, distinct from a field
// that was simply absent.
type rejection struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func main() {
	app := gofr.New()

	app.POST("/extract", extract)

	app.Run()
}

func extract(c *gofr.Context) (any, error) {
	var in struct {
		Text   string      `json:"text"`
		Fields []fieldSpec `json:"fields"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	text := strings.TrimSpace(in.Text)
	if text == "" {
		return map[string]any{
			"error": "provide some text to extract from, and optionally a `fields` schema " +
				"(each field has a name and a type: string, integer, number, boolean, date, or a " +
				"list like string[]); with no schema I extract common entities.",
		}, nil
	}

	if len(text) > maxTextChars {
		text = text[:maxTextChars]
	}

	schema := in.Fields
	if len(schema) == 0 {
		schema = defaultSchema
	}

	if bad := validateSchema(schema); bad != "" {
		return map[string]any{"error": bad}, nil
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a precise information-extraction engine. Extract the " +
			"requested fields from the user's text and reply with ONLY a single JSON object whose keys " +
			"are exactly the requested field names — no markdown, no prose, no extra keys. Use the value " +
			"null (or [] for a list) for anything the text does not state. Never invent a value that is " +
			"not supported by the text."},
		{Role: ai.RoleUser, Content: "Fields to extract:\n" + describeSchema(schema) +
			"\n\nText:\n" + text},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{
			"schema": schema,
			"error":  "model did not return a JSON object: " + err.Error(),
		}, nil
	}

	fields, missing, dropped, rejected := validate(schema, raw)

	return map[string]any{
		"text_chars": len(text),
		"schema":     schema,
		"fields":     fields,   // validated, correctly typed values — safe to consume
		"missing":    missing,  // required fields that never resolved to a valid value
		"dropped":    dropped,  // keys the model returned that weren't in the schema
		"rejected":   rejected, // requested fields whose value failed type validation
	}, nil
}

// describeSchema renders the schema for the prompt: one line per field with its type, whether it's
// required, and any description.
func describeSchema(schema []fieldSpec) string {
	var b strings.Builder

	for _, f := range schema {
		b.WriteString("- ")
		b.WriteString(f.Name)
		b.WriteString(" (")
		b.WriteString(f.Type)

		if f.Required {
			b.WriteString(", required")
		}

		b.WriteString(")")

		if f.Description != "" {
			b.WriteString(": ")
			b.WriteString(f.Description)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// validateSchema returns a human-readable reason if any field spec is unusable (empty name, or a
// type that isn't a supported scalar or "<scalar>[]" list), else "".
func validateSchema(schema []fieldSpec) string {
	seen := make(map[string]bool, len(schema))

	for _, f := range schema {
		if strings.TrimSpace(f.Name) == "" {
			return "every field needs a non-empty name"
		}

		if seen[f.Name] {
			return "duplicate field name: " + f.Name
		}

		seen[f.Name] = true

		if !scalarTypes[baseType(f.Type)] {
			return fmt.Sprintf("field %q has unsupported type %q (use string, integer, number, "+
				"boolean, date, or a list like string[])", f.Name, f.Type)
		}
	}

	return ""
}

// validate is the guardrail: it takes the schema and the model's parsed JSON object and returns only
// values that survive type validation. It never trusts the model's output shape — keys not in the
// schema are dropped, values that don't match their declared type are rejected, and required fields
// that didn't resolve to a valid value are reported missing.
func validate(schema []fieldSpec, raw map[string]any) (fields map[string]any, missing, dropped []string, rejected []rejection) {
	fields = make(map[string]any)
	missing = []string{}
	dropped = []string{}
	rejected = []rejection{}

	want := make(map[string]bool, len(schema))
	for _, f := range schema {
		want[f.Name] = true
	}

	for k := range raw {
		if !want[k] {
			dropped = append(dropped, k)
		}
	}

	for _, f := range schema {
		v, present := raw[f.Name]

		if !present || v == nil {
			if f.Required {
				missing = append(missing, f.Name)
			}

			continue
		}

		coerced, ok := coerce(v, f.Type)
		if !ok {
			rejected = append(rejected, rejection{Field: f.Name, Reason: fmt.Sprintf(
				"value %v is not a valid %s", v, f.Type)})

			if f.Required {
				missing = append(missing, f.Name)
			}

			continue
		}

		fields[f.Name] = coerced
	}

	return fields, missing, dropped, rejected
}

// coerce validates a single value against a declared type, returning the normalized value and
// whether it is valid. A list type ("<scalar>[]") requires a JSON array and coerces each element,
// keeping only the ones that validate; an empty result counts as invalid so a required list that
// produced nothing is reported missing rather than as an empty success.
func coerce(v any, typ string) (any, bool) {
	if isList(typ) {
		arr, ok := v.([]any)
		if !ok {
			return nil, false
		}

		out := make([]any, 0, len(arr))

		for _, e := range arr {
			if cv, ok := coerceScalar(e, baseType(typ)); ok {
				out = append(out, cv)
			}
		}

		if len(out) == 0 {
			return nil, false
		}

		return out, true
	}

	return coerceScalar(v, typ)
}

// coerceScalar validates one non-list value against a scalar type. JSON numbers arrive as float64,
// so integer/number accept a numeric value or a numeric string; boolean accepts a bool or a common
// truthy/falsy word; date accepts any of dateLayouts and normalizes to YYYY-MM-DD.
func coerceScalar(v any, typ string) (any, bool) {
	switch typ {
	case "string":
		s, ok := stringOf(v)
		return s, ok && s != ""

	case "integer":
		f, ok := numberOf(v)
		if !ok || f != float64(int64(f)) {
			return nil, false
		}

		return int64(f), true

	case "number":
		return numberOf(v)

	case "boolean":
		return boolOf(v)

	case "date":
		s, ok := v.(string)
		if !ok {
			return nil, false
		}

		for _, layout := range dateLayouts {
			if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
				return t.Format("2006-01-02"), true
			}
		}

		return nil, false

	default:
		return nil, false
	}
}

// stringOf accepts a string as-is (trimmed) and renders a JSON number or bool as its string form, so
// a "string" field tolerates a model that answered with a bare number.
func stringOf(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	default:
		return "", false
	}
}

// numberOf accepts a JSON number or a numeric string.
func numberOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// boolOf accepts a real bool or a common truthy/falsy word.
func boolOf(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "y", "1":
			return true, true
		case "false", "no", "n", "0":
			return false, true
		}
	}

	return false, false
}

func isList(typ string) bool { return strings.HasSuffix(typ, "[]") }

func baseType(typ string) string { return strings.TrimSuffix(typ, "[]") }

// extractJSON pulls a single JSON object out of a model response that — despite instructions — may
// wrap it in markdown fences or prose. It finds the first '{' and its balanced closing '}' (ignoring
// braces inside strings), then unmarshals that slice.
func extractJSON(s string) (map[string]any, error) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found")
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
			// inside a string literal — braces don't count
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				var m map[string]any
				if err := json.Unmarshal([]byte(s[start:i+1]), &m); err != nil {
					return nil, err
				}

				return m, nil
			}
		}
	}

	return nil, fmt.Errorf("unbalanced JSON object")
}
