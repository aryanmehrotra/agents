# support-agent

Triage an incoming support ticket (or GitHub issue) with an LLM, and stream a drafted reply over SSE.
Two everyday LLM patterns: a **structured one-shot** classification and **token streaming**.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/triage` | classify a ticket → JSON `{type, priority, summary, needs_info, suggested_reply}` |
| POST | `/triage/stream` | stream a drafted reply token-by-token (SSE) |

## Run

```bash
# keyless (start ../../../localtest/claude-openai-shim first)
cp configs/.env.local configs/.env && go run .

# or with Groq
cp configs/.env.example configs/.env   # add GROQ_API_KEY
go run .
```

## Try it

```bash
curl -s localhost:8001/triage -d '{
  "title":"App crashes on login after 1.58 upgrade",
  "body":"Every login throws a nil pointer panic. Prod is down for all users."
}'
# → {"type":"bug","priority":"P0","summary":"…","needs_info":false,"suggested_reply":"…"}

# streamed reply (note -N for no buffering)
curl -sN localhost:8001/triage/stream -d '{"title":"How do I rotate my API key?","body":"Cant find it in the docs."}'
```

The classification prompt lives in `main.go` (`triageSystem`) — tune the labels/priorities to your
own workflow.
