# summarizer-agent

Summarizes a long document, email thread or chat transcript with an LLM: a **structured
breakdown** (tl;dr, key points, action items, decisions, open questions) plus a **streamed
narrative summary** for quick reading. Long-document / thread summarization is one of the
most common tightly-scoped sub-agent responsibilities teams wire behind a router — the same
structured-JSON + streaming split as `support-agent` and `pii-redaction-agent`.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/summarize` | structured breakdown → JSON `{tl_dr, key_points[], action_items[], decisions[], open_questions[]}` |
| POST | `/summarize/stream` | streamed narrative summary, in prose (SSE) |

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
curl -s localhost:8005/summarize -d '{
  "text": "Thread: Q3 launch planning...\nAlice: we need the pricing page done by Friday.\nBob: I will ship the pricing page, but I need the final tier names from Carol.\nCarol: tiers are Starter/Pro/Scale, confirmed with finance.\nAlice: great, then we launch Monday unless legal has concerns."
}'
# → {"tl_dr":"Team aligned on pricing tiers and a Monday launch pending legal sign-off.",
#    "key_points":["Pricing tiers: Starter/Pro/Scale, confirmed with finance"],
#    "action_items":["Bob: ship pricing page by Friday using final tier names"],
#    "decisions":["Tier names finalized as Starter/Pro/Scale"],
#    "open_questions":["Does legal have any concerns before Monday launch?"]}

# streamed narrative summary (note -N for no buffering)
curl -sN localhost:8005/summarize/stream -d '{"text":"..."}'
```

The summarization prompt lives in `main.go` (`summarizeSystem`) — adjust the JSON shape or
add categories (e.g. `risks`, `owners`) to fit your own workflow.
