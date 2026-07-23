# pii-redaction-agent

Detects personally identifiable information in text with an LLM and returns a **redacted
version plus a structured entity list** — the compliance/redaction pattern enterprises put in
front of support tickets, chat logs and documents before they hit storage, logs or a
third-party model. The LLM only *detects* entities; the actual substitution happens
deterministically in Go, so a hallucinated span can't corrupt the redacted output.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/redact` | structured redaction → JSON `{redacted_text, entities:[{type, value}], risk}` |
| POST | `/redact/stream` | streamed compliance rationale — what PII categories were found and why they matter (SSE) |

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
curl -s localhost:8004/redact -d '{
  "text": "Hi, this is John Doe. My email is john.doe@example.com and my SSN is 123-45-6789."
}'
# → {"redacted_text":"Hi, this is [NAME]. My email is [EMAIL] and my SSN is [SSN].",
#    "entities":[{"type":"name","value":"John Doe"},{"type":"email","value":"john.doe@example.com"},{"type":"ssn","value":"123-45-6789"}],
#    "risk":"high"}

# streamed compliance rationale (note -N for no buffering)
curl -sN localhost:8004/redact/stream -d '{"text":"..."}'
```

The detection prompt lives in `main.go` (`detectSystem`) — extend the entity types or add
house redaction rules (e.g. partial masking instead of full replacement) to fit your own
compliance policy.
