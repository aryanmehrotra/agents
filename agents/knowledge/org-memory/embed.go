package main

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Embedder turns text into a vector. It is an interface so tests inject a deterministic, offline
// fake and production wraps the LLM embedder (ctx.LLM("embed").Embed). Embedding is the ONLY model
// dependency on the write path and is kept off the hot ranking path.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// fakeEmbedder is a deterministic, dependency-free embedder for tests and offline/local runs:
// hashed bag-of-words into a fixed-dim L2-normalized vector. Shared words → higher cosine, so
// ranking/precision tests are meaningful without a live model.
type fakeEmbedder struct{ dim int }

// dim is large so distinct words rarely collide into the same bucket — otherwise unrelated text
// would show spurious similarity (a bag-of-words artifact, not real semantics). Deterministic.
func newFakeEmbedder() fakeEmbedder { return fakeEmbedder{dim: 4096} }

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dim := f.dim
	if dim <= 0 {
		dim = 64
	}

	v := make([]float32, dim)

	for _, tok := range tokenize(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		v[h.Sum32()%uint32(dim)]++
	}

	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}

	if n > 0 {
		s := float32(1 / math.Sqrt(n))
		for i := range v {
			v[i] *= s
		}
	}

	return v, nil
}

// tokenize lowercases and splits on non-alphanumeric runes.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
