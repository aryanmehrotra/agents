# data-agent

Ask your own microservice in natural language. `data-agent` exposes its read endpoints as **MCP
tools** with `app.EnableMCP()`, and its `/ask` handler runs an **agent loop** with
`ctx.LLM().Tools()` — the model answers by calling the service's real endpoints (binding, validation,
auth and observability all run; one coherent trace per request).

The domain is a tiny in-memory storefront, so there's no database to set up.

## Endpoints

| Method | Path | Purpose | MCP tool? |
|--------|------|---------|-----------|
| GET | `/products?category=` | list products | ✅ |
| GET | `/products/{id}` | one product | ✅ |
| GET | `/orders?status=` | list orders | ✅ |
| GET | `/stats` | revenue / stock summary | ✅ |
| POST | `/ask` | agent loop over the tools above | — |

Only `GET`/`HEAD`/`OPTIONS` handlers are exposed as tools, and the MCP server binds to loopback — an
agent can read through this surface but never mutate.

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
# hit a tool endpoint directly
curl -s localhost:8000/stats

# let the agent figure it out
curl -s localhost:8000/ask -d '{"question":"Which category has the most stock, and list any pending orders."}'
```

Watch the logs: one `/ask` fans out into several `chat/completions` + tool-call round-trips, all under
one correlation ID.

## Point a real MCP client at it

With the service running, its tools are live on `MCP_PORT` (default 8200) — connect Claude Code /
Claude Desktop or any MCP client to explore `get_products`, `get_stats`, etc.
