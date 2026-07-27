package main

import (
	"reflect"
	"testing"
)

// TestExtractJSON covers the pure logic that recovers a JSON object from a model response, which —
// despite instructions — may fence it, prefix it with prose, or contain braces inside strings.
func TestExtractJSON(t *testing.T) {
	ok := []struct {
		name string
		in   string
		want map[string]any
	}{
		{"plain", `{"a":"b"}`, map[string]any{"a": "b"}},
		{"fenced", "```json\n{\"a\":1}\n```", map[string]any{"a": float64(1)}},
		{"prose around it", `Sure! Here you go:\n{"a":true} — hope that helps`, map[string]any{"a": true}},
		{"brace inside string", `{"note":"a } b { c"}`, map[string]any{"note": "a } b { c"}},
		{"nested", `{"x":{"y":1}}`, map[string]any{"x": map[string]any{"y": float64(1)}}},
	}

	for _, tc := range ok {
		got, err := extractJSON(tc.in)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: extractJSON() = %v, want %v", tc.name, got, tc.want)
		}
	}

	bad := []string{"no json here", "", `{"a":`, "just ] brackets"}
	for _, in := range bad {
		if _, err := extractJSON(in); err == nil {
			t.Errorf("extractJSON(%q) expected error, got nil", in)
		}
	}
}

// TestCoerceScalar is the core of the guardrail: every value the model returns is checked against
// its declared type, and only well-typed values survive.
func TestCoerceScalar(t *testing.T) {
	cases := []struct {
		typ  string
		in   any
		want any
		ok   bool
	}{
		{"string", "  hi ", "hi", true},
		{"string", "", nil, false},
		{"string", float64(42), "42", true}, // model answered a number for a string field
		{"integer", float64(7), int64(7), true},
		{"integer", float64(7.5), nil, false}, // not integral
		{"integer", "12", int64(12), true},
		{"integer", "nope", nil, false},
		{"number", float64(3.14), float64(3.14), true},
		{"number", "2.5", float64(2.5), true},
		{"boolean", true, true, true},
		{"boolean", "yes", true, true},
		{"boolean", "0", false, true},
		{"boolean", "maybe", nil, false},
		{"date", "2026-07-27", "2026-07-27", true},
		{"date", "07/27/2026", "2026-07-27", true}, // normalized
		{"date", "not a date", nil, false},
		{"date", float64(20260727), nil, false}, // dates must be strings
	}

	for _, tc := range cases {
		got, ok := coerceScalar(tc.in, tc.typ)
		if ok != tc.ok {
			t.Errorf("coerceScalar(%v, %q) ok = %v, want %v", tc.in, tc.typ, ok, tc.ok)
			continue
		}

		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("coerceScalar(%v, %q) = %v, want %v", tc.in, tc.typ, got, tc.want)
		}
	}
}

// TestCoerceList checks list handling: a list requires a JSON array, coerces each element, drops the
// ones that don't validate, and treats an all-invalid (or empty) list as invalid.
func TestCoerceList(t *testing.T) {
	got, ok := coerce([]any{"a", "", "b"}, "string[]")
	if !ok || !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf(`coerce(["a","","b"], "string[]") = %v,%v want ["a","b"],true`, got, ok)
	}

	got, ok = coerce([]any{float64(1), float64(2.5), float64(3)}, "integer[]")
	if !ok || !reflect.DeepEqual(got, []any{int64(1), int64(3)}) {
		t.Errorf(`coerce([1,2.5,3], "integer[]") = %v,%v want [1,3],true`, got, ok)
	}

	if _, ok := coerce("not an array", "string[]"); ok {
		t.Errorf(`coerce("not an array", "string[]") should be invalid`)
	}

	if _, ok := coerce([]any{"x", "y"}, "integer[]"); ok {
		t.Errorf(`coerce(["x","y"], "integer[]") should be invalid (no valid elements)`)
	}
}

// TestValidate is the end-to-end guardrail: given a schema and a model's parsed object, only
// well-typed requested fields survive; unknown keys are dropped; wrong-typed values are rejected;
// required fields that never resolved are reported missing.
func TestValidate(t *testing.T) {
	schema := []fieldSpec{
		{Name: "invoice_no", Type: "string", Required: true},
		{Name: "total", Type: "number", Required: true},
		{Name: "due", Type: "date"},
		{Name: "paid", Type: "boolean"},
		{Name: "line_items", Type: "string[]"},
	}

	raw := map[string]any{
		"invoice_no":   "INV-1001",
		"total":        "1299.50",       // numeric string → coerced
		"due":          "not-a-date",    // present but invalid → rejected + (not required, so not missing)
		"paid":         "yes",           // → true
		"line_items":   []any{"widget"}, // valid list
		"vendor_email": "a@b.com",       // not in schema → dropped
	}

	fields, missing, dropped, rejected := validate(schema, raw)

	wantFields := map[string]any{
		"invoice_no": "INV-1001",
		"total":      float64(1299.50),
		"paid":       true,
		"line_items": []any{"widget"},
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Errorf("fields = %v, want %v", fields, wantFields)
	}

	if !reflect.DeepEqual(dropped, []string{"vendor_email"}) {
		t.Errorf("dropped = %v, want [vendor_email]", dropped)
	}

	if len(rejected) != 1 || rejected[0].Field != "due" {
		t.Errorf("rejected = %v, want one rejection for 'due'", rejected)
	}

	// 'due' is not required, so it isn't missing; all required fields resolved.
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
}

// TestValidateMissingRequired: a required field that's absent, null, or invalidly typed is reported
// missing — the whole point of the required flag.
func TestValidateMissingRequired(t *testing.T) {
	schema := []fieldSpec{
		{Name: "a", Type: "string", Required: true},
		{Name: "b", Type: "integer", Required: true},
		{Name: "c", Type: "string", Required: true},
	}

	raw := map[string]any{
		"a": nil,      // null → missing
		"b": "twelve", // invalid → rejected AND missing (required)
		// c absent → missing
	}

	_, missing, _, rejected := validate(schema, raw)

	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(missing) != 3 {
		t.Fatalf("missing = %v, want a,b,c", missing)
	}

	for _, m := range missing {
		if !want[m] {
			t.Errorf("unexpected missing field %q", m)
		}
	}

	if len(rejected) != 1 || rejected[0].Field != "b" {
		t.Errorf("rejected = %v, want one for 'b'", rejected)
	}
}

// TestValidateSchema rejects unusable schemas before any model call.
func TestValidateSchema(t *testing.T) {
	good := []fieldSpec{{Name: "x", Type: "string"}, {Name: "y", Type: "number[]"}, {Name: "z", Type: "date"}}
	if msg := validateSchema(good); msg != "" {
		t.Errorf("validateSchema(good) = %q, want ''", msg)
	}

	bad := [][]fieldSpec{
		{{Name: "", Type: "string"}},                               // empty name
		{{Name: "x", Type: "float"}},                               // unsupported type
		{{Name: "x", Type: "string"}, {Name: "x", Type: "number"}}, // duplicate
		{{Name: "x", Type: "object[]"}},                            // unsupported base type in list
	}
	for i, schema := range bad {
		if msg := validateSchema(schema); msg == "" {
			t.Errorf("validateSchema(bad[%d]) = '', want a reason", i)
		}
	}
}
