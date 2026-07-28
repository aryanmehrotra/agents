// kb-agent — a GoFr 1.58 retrieval-augmented helpdesk. It loads a small knowledge
// base from ./kb at startup, retrieves the most relevant chunks for a question, and
// asks the LLM to answer grounded in them (with sources). The retriever here is
// deliberately simple lexical overlap so the demo needs no vector store or embedding
// provider — swap `retrieve` for a real vector search in production.
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/http/response"
)

const topK = 3

type chunk struct {
	Source string
	Text   string
}

var kb []chunk

func main() {
	app := gofr.New()

	loadKB(app.Logger(), "kb")

	app.POST("/ask", ask)
	app.POST("/ask/stream", askStream)

	app.Run()
}

func ask(c *gofr.Context) (any, error) {
	var in struct {
		Question string `json:"question"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	hits := retrieve(in.Question, topK)

	resp, err := c.LLM().Chat(c, messagesFor(in.Question, hits), ai.WithTemperature(0.2))
	if err != nil {
		return nil, err
	}

	return map[string]any{"answer": resp.Content, "sources": sourcesOf(hits)}, nil
}

func askStream(c *gofr.Context) (any, error) {
	var in struct {
		Question string `json:"question"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	stream, err := c.LLM().Stream(c, messagesFor(in.Question, retrieve(in.Question, topK)))
	if err != nil {
		return nil, err
	}

	return response.Stream{Source: stream}, nil
}

func messagesFor(question string, hits []chunk) []ai.Message {
	var ctx strings.Builder
	for _, h := range hits {
		ctx.WriteString("### " + h.Source + "\n" + h.Text + "\n\n")
	}

	return []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an internal IT/HR helpdesk assistant. Answer ONLY from the provided context. " +
			"If the answer isn't in the context, say you don't have that information and suggest who to contact. Be concise."},
		{Role: ai.RoleUser, Content: "Context:\n" + ctx.String() + "\nQuestion: " + question},
	}
}

// retrieve scores each chunk by how many distinct query words it contains and returns the top n.
func retrieve(query string, n int) []chunk {
	terms := tokenize(query)

	type scored struct {
		c chunk
		s int
	}

	ranked := make([]scored, 0, len(kb))
	for _, ch := range kb {
		text := strings.ToLower(ch.Text)
		score := 0
		for term := range terms {
			if strings.Contains(text, term) {
				score++
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{ch, score})
		}
	}

	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].s > ranked[j].s })

	out := make([]chunk, 0, n)
	for i := 0; i < len(ranked) && i < n; i++ {
		out = append(out, ranked[i].c)
	}

	return out
}

func tokenize(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) > 2 { // skip short stopwords-ish tokens
			set[w] = struct{}{}
		}
	}
	return set
}

func sourcesOf(hits []chunk) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, h := range hits {
		if !seen[h.Source] {
			seen[h.Source] = true
			out = append(out, h.Source)
		}
	}
	return out
}

// loadKB reads every .md file under dir and splits it into paragraph chunks.
func loadKB(log logger, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Errorf("kb: cannot read %s: %v", dir, err)
		return
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}

		for _, para := range strings.Split(string(data), "\n\n") {
			para = strings.TrimSpace(para)
			if len(para) > 0 {
				kb = append(kb, chunk{Source: e.Name(), Text: para})
			}
		}
	}

	log.Infof("kb: loaded %d chunks from %s", len(kb), dir)
}

type logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}
