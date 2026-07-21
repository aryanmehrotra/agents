# claude-openai-shim

A tiny **OpenAI-compatible endpoint backed by the local `claude` CLI**, so the agents in this repo
run end-to-end with **no API key and no model download**. It's a test harness, not a production
gateway.

## Run

```bash
go run .        # listens on :8088  (override with SHIM_ADDR)
```

Then point any agent at it:

```dotenv
LLM_PROVIDER=openai
LLM_BASE_URL=http://localhost:8088/v1
LLM_API_KEY=anything
```

(That's exactly what each agent's `configs/.env.local` already does.)

## What it does

- Serves `POST /v1/chat/completions` (streaming and non-streaming) and `GET /v1/models`.
- Flattens the OpenAI message array into a prompt and runs `claude -p … --output-format json`.
- Maps the CLI's real token usage into the `usage` field, so GoFr's token metrics show real numbers.
- **Tool calls:** when a request includes `tools`, it asks the model for a strict-JSON decision and
  surfaces it as an OpenAI `tool_calls` response — enough to drive `data-agent`'s agent loop.

## Limitations

It shells out to the CLI (slower than a real API), fakes token-streaming by chunking the final text,
and its tool-calling is best-effort JSON coaxing. Use a real provider (Groq / OpenAI / Ollama) for
anything beyond local testing.
