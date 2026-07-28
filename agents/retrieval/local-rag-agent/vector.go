// vector.go stores and searches document chunks in SurrealDB — used here as a vector database
// through GoFr's SurrealDB datasource (c.SurrealDB). Each chunk is stored with its embedding, and
// recall ranks chunks by cosine similarity to a query vector.
package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
)

// chunk is one stored, embedded passage plus its similarity score when returned from a search.
type chunk struct {
	Source  string  `json:"source"`
	Content string  `json:"content"`
	Score   float64 `json:"score,omitempty"`
}

// storeChunk writes one embedded chunk to SurrealDB via GoFr's datasource. CREATE ... CONTENT is
// issued through Query (SurrealDB returns an array the typed Create helper can't decode into a map).
func storeChunk(c *gofr.Context, source, content string, vec []float32) error {
	_, err := c.SurrealDB.Query(c, "CREATE document CONTENT $d", map[string]any{
		"d": map[string]any{
			"source":    source,
			"content":   content,
			"embedding": vec,
			"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		},
	})

	return err
}

// searchChunks returns the k chunks most similar to qvec, above minScore. The query vector is
// inlined as a SurrealQL array literal (a bound var is driver-serialized into a value the vector
// function rejects), and cosine is scaled to an integer with math::round(sim*10000) because the
// driver decodes a bare float score as 0 — it's scaled back in Go.
func searchChunks(c *gofr.Context, qvec []float32, k int, minScore float64) []chunk {
	query := fmt.Sprintf(
		"SELECT source, content, "+
			"math::round(vector::similarity::cosine(embedding, %s) * 10000) AS score "+
			"FROM document ORDER BY score DESC LIMIT %d",
		floatLiteral(qvec), k)

	rows, err := c.SurrealDB.Query(c, query, map[string]any{})
	if err != nil {
		c.Logger.Errorf("local-rag: vector search failed: %v", err)
		return nil
	}

	out := toChunks(rows)

	kept := out[:0]
	for i := range out {
		out[i].Score /= 10000 // undo the integer scaling
		if out[i].Score >= minScore {
			kept = append(kept, out[i])
		}
	}

	return kept
}

// toChunks decodes SurrealDB's generic rows into chunks, tolerating the several numeric types the
// driver may use for the scaled score.
func toChunks(rows []any) []chunk {
	out := make([]chunk, 0, len(rows))

	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}

		var ch chunk
		if v, ok := m["source"].(string); ok {
			ch.Source = v
		}

		if v, ok := m["content"].(string); ok {
			ch.Content = v
		}

		switch v := m["score"].(type) {
		case float64:
			ch.Score = v
		case float32:
			ch.Score = float64(v)
		case int64:
			ch.Score = float64(v)
		case int:
			ch.Score = float64(v)
		case uint64:
			ch.Score = float64(v)
		}

		out = append(out, ch)
	}

	return out
}

// floatLiteral renders a vector as a SurrealQL array literal, e.g. [0.12,-0.03,…].
func floatLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', 6, 32)
	}

	return "[" + strings.Join(parts, ",") + "]"
}
