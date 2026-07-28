package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestChunkText covers the pure chunking logic that decides what gets embedded and stored: whole
// paragraphs are packed up to the limit, an over-long paragraph is split, and every chunk stays
// within the size bound.
func TestChunkText(t *testing.T) {
	if got := chunkText("   ", 100); got != nil {
		t.Errorf("chunkText(blank) = %v, want nil", got)
	}

	// Three short paragraphs, limit big enough to pack the first two but not all three.
	text := "alpha para one.\n\nbeta para two.\n\ngamma the third paragraph here."
	got := chunkText(text, 35)
	for _, c := range got {
		if len(c) > 35 {
			t.Errorf("chunk exceeds limit (%d): %q", len(c), c)
		}
	}

	joined := strings.Join(got, " ")
	for _, w := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(joined, w) {
			t.Errorf("chunkText dropped content %q; got %v", w, got)
		}
	}
}

// TestChunkTextLongParagraph: a single paragraph larger than the limit is split into bounded pieces
// on word boundaries (no piece exceeds the limit, no word is cut).
func TestChunkTextLongParagraph(t *testing.T) {
	para := strings.TrimSpace(strings.Repeat("word ", 100)) // 500 chars, no blank lines
	got := chunkText(para, 80)

	if len(got) < 2 {
		t.Fatalf("expected the long paragraph to split into multiple chunks, got %d", len(got))
	}

	for _, c := range got {
		if len(c) > 80 {
			t.Errorf("chunk exceeds limit (%d): %q", len(c), c)
		}

		for _, tok := range strings.Fields(c) {
			if tok != "word" {
				t.Errorf("word boundary broken, found token %q in %q", tok, c)
			}
		}
	}
}

// TestFloatLiteral checks the SurrealQL array literal used to inline a query vector.
func TestFloatLiteral(t *testing.T) {
	got := floatLiteral([]float32{0.5, -0.25, 1})
	want := "[0.500000,-0.250000,1.000000]"

	if got != want {
		t.Errorf("floatLiteral() = %q, want %q", got, want)
	}

	if floatLiteral(nil) != "[]" {
		t.Errorf("floatLiteral(nil) = %q, want []", floatLiteral(nil))
	}
}

// TestToChunks verifies SurrealDB rows decode into chunks, including the several numeric types the
// driver may hand back for the scaled score.
func TestToChunks(t *testing.T) {
	rows := []any{
		map[string]any{"source": "doc-a", "content": "hello", "score": int64(9000)},
		map[string]any{"source": "doc-b", "content": "world", "score": float64(4200)},
		"not-a-row", // ignored
	}

	got := toChunks(rows)
	want := []chunk{
		{Source: "doc-a", Content: "hello", Score: 9000},
		{Source: "doc-b", Content: "world", Score: 4200},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("toChunks() = %+v, want %+v", got, want)
	}
}

// TestEnvHelpers covers the small config helpers that keep the agent generic (env-overridable).
func TestEnvHelpers(t *testing.T) {
	if got := envInt("LOCAL_RAG_NOPE", 7); got != 7 {
		t.Errorf("envInt(default) = %d, want 7", got)
	}

	t.Setenv("LOCAL_RAG_K", "12")
	if got := envInt("LOCAL_RAG_K", 7); got != 12 {
		t.Errorf("envInt(set) = %d, want 12", got)
	}

	if got := envBool("LOCAL_RAG_NOPE", true); !got {
		t.Errorf("envBool(default) = %v, want true", got)
	}

	t.Setenv("LOCAL_RAG_THINK", "false")
	if got := envBool("LOCAL_RAG_THINK", true); got {
		t.Errorf("envBool(set) = %v, want false", got)
	}
}
