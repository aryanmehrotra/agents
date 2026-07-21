# kb-agent

An internal IT/HR helpdesk that answers **grounded in your docs** (retrieval-augmented generation).
It loads a knowledge base from [`kb/`](kb) at startup, retrieves the most relevant chunks for a
question, and asks the LLM to answer only from them — returning the sources it used.

The retriever here is deliberately simple lexical overlap so the demo needs **no vector store or
embedding provider**. For production, swap the `retrieve` function in `main.go` for a real vector
search (pgvector, Qdrant, etc.) — the rest stays the same.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/ask` | answer from the KB → `{answer, sources}` |
| POST | `/ask/stream` | same, streamed over SSE |

## Knowledge base

Drop any `.md` files into [`kb/`](kb); they're split into paragraph chunks at startup. Shipped
samples: `it-helpdesk.md`, `hr-policies.md`.

## Run

```bash
# keyless (start ../localtest/claude-openai-shim first)
cp configs/.env.local configs/.env && go run .

# or with Groq
cp configs/.env.example configs/.env   # add GROQ_API_KEY
go run .
```

## Try it

```bash
curl -s localhost:8002/ask -d '{"question":"How many days of annual leave do I get and how much carries over?"}'
# → {"answer":"You get 20 days … up to 5 unused days carry over …","sources":["hr-policies.md"]}

curl -s localhost:8002/ask -d '{"question":"I lost my MFA device, what do I do?"}'
```

Ask something outside the KB and it will tell you it doesn't have that information rather than
hallucinate — that's the grounding system prompt doing its job.
