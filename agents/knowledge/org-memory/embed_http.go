package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Asymmetric prefixes real embedders (e.g. nomic-embed-text) expect: stored decisions are documents,
// recall inputs are queries. Embedding both the same way measurably hurts retrieval.
const (
	docPrefix   = "search_document: "
	queryPrefix = "search_query: "
)

var errNoEmbedding = errors.New("embeddings endpoint returned no vector")

// httpEmbedder produces REAL semantic embeddings by calling any OpenAI-compatible /v1/embeddings
// endpoint — Ollama (nomic-embed-text) for a local, private, semantic embedder, your orchestrator, or
// a hosted provider. It is decoupled from GoFr's (forked) embeddings capability and uses plain HTTP +
// context.Context. There is deliberately NO silent fallback to the local embedder: mixing a real
// provider's vectors with local ones would give mismatched dimensions and break cosine, and a down
// provider should fail loudly. The local embedder stays an *explicit* offline mode (ORGMEM_EMBED=local).
type httpEmbedder struct {
	baseURL string // e.g. http://localhost:11434/v1
	model   string
	apiKey  string
	client  *http.Client
}

func newHTTPEmbedder(baseURL, model, apiKey string) httpEmbedder {
	return httpEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type embRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (e httpEmbedder) Embed(ctx context.Context, text string, role EmbedRole) ([]float32, error) {
	prefix := docPrefix
	if role == RoleQuery {
		prefix = queryPrefix
	}

	body, err := json.Marshal(embRequest{Model: e.model, Input: []string{prefix + text}})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out embResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding embeddings response: %w", err)
	}

	if out.Error != nil {
		return nil, fmt.Errorf("embeddings provider: %s", out.Error.Message)
	}

	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("embeddings provider returned status %d", resp.StatusCode)
	}

	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, errNoEmbedding
	}

	return out.Data[0].Embedding, nil
}
