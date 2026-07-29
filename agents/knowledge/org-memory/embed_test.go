package main

import (
	"context"
	"testing"
)

// The fake embedder must give related text higher cosine than unrelated text, otherwise the
// capture/recall tests that depend on it would be meaningless.
func TestFakeEmbedderSimilarity(t *testing.T) {
	e := newFakeEmbedder()
	ctx := context.Background()

	pay1, _ := e.Embed(ctx, "money must be stored as int64 cents")
	pay2, _ := e.Embed(ctx, "store money in int64 cents never float")
	css, _ := e.Embed(ctx, "css flexbox layout for the sidebar")

	related := cosine(pay1, pay2)
	unrelated := cosine(pay1, css)

	if related <= unrelated {
		t.Fatalf("related text should be more similar than unrelated: related=%v unrelated=%v", related, unrelated)
	}
}
