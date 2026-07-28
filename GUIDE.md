# Customising the agents & composing them together

Two things make this repo a *platform* rather than a pile of demos: every agent talks to its model
through the **same seam**, and every agent is reachable over the **same resilient transport**. That's
what lets you (1) reconfigure any agent without touching its code, and (2) wire agents together into
larger flows. This guide covers both.

---

## Part 1 — Customising any agent

Every agent is its own Go module with a `configs/.env.example` (and a keyless `configs/.env.local`).
You customise by copying one to `configs/.env` and editing it — **no code changes**.

### 1. Swap the model or provider (one line)

Every agent reaches its LLM through the exact same call — `ctx.LLM().Chat(...)` / `.Generate(...)` /
`.Embed(...)`. It never knows or cares which provider is behind it. So the provider is pure config:

| Provider | `.env` |
|----------|--------|
| 🟢 **Groq** *(default in `.env.example`)* | `LLM_PROVIDER=groq` · `LLM_MODEL=llama-3.3-70b-versatile` · `GROQ_API_KEY=…` |
| ⚪ **OpenAI** | `LLM_PROVIDER=openai` · `LLM_MODEL=gpt-4o-mini` · `OPENAI_API_KEY=…` |
| 🦙 **Ollama** *(local)* | `LLM_PROVIDER=ollama` · `LLM_MODEL=llama3.2` · `LLM_BASE_URL=http://localhost:11434/v1` |
| 🧪 **claude-CLI shim** *(keyless, this repo)* | `LLM_PROVIDER=openai` · `LLM_BASE_URL=http://localhost:8088/v1` · any key |
| 🔒 **in-process llama.cpp** *(`local-rag-agent` only)* | `CHAT_MODEL` / `EMBED_MODEL` = any GGUF — see that agent's README |

The API key is read from the generic `LLM_API_KEY` first, then the provider-specific variable
(`OPENAI_API_KEY`, `GROQ_API_KEY`, …), so an existing key in your environment just works. Point
`LLM_BASE_URL` at *any* OpenAI-compatible endpoint (a gateway, an aggregator, a self-hosted server).

Agents that use **two** models (`memory-agent`, `local-rag-agent`) split it into `CHAT_MODEL` and
`EMBED_MODEL` (embeddings are usually a different model than chat).

### 2. Point it at your own data

The stateful agents take their datasource from config too — GoFr wires the handle up for you:

- `sql-agent`: `DB_DIALECT` / `DB_NAME` (SQLite locally; set `mysql`/`postgres` + `DB_HOST`/`DB_USER`/`DB_PASSWORD` for a real warehouse).
- `memory-agent`, `local-rag-agent`: `SURREAL_HOST` / `SURREAL_PORT` / `SURREAL_USER` / `SURREAL_PASS` / `SURREAL_NS` / `SURREAL_DB`.
- `kb-agent`: drop your documents in its `./kb` folder.

### 3. Tune behaviour and the guardrails

Every agent pairs the model with a **deterministic Go guardrail** — that's the part you audit and
tune, because it's plain code, not a prompt:

| Agent | The knob you can change |
|-------|--------------------------|
| `sql-agent` | `isSafeSelect` / `forbiddenKeywords` — what SQL is allowed to run |
| `research-agent`, `scheduler-agent` | `isSafeURL` / `isBlockedIP` — the SSRF allowlist for outbound calls |
| `extraction-agent` | the field types + `validate` split (dropped / rejected / missing) |
| `local-rag-agent` | `RECALL_FLOOR`, `CHAT_MAX_TOKENS`, `ENABLE_THINKING`, chunk size |
| `scheduler-agent` | `maxDelaySeconds`, `fireTimeout` |

Per-call model options are set in the handler with `ai.WithTemperature(…)`, `ai.WithMaxTokens(…)`.

### 4. Add your own agent (the recipe)

An agent is ~50 lines plus wiring. The whole contract:

```go
func main() {
    app := gofr.New()
    app.POST("/do", func(c *gofr.Context) (any, error) {
        var in struct{ Text string `json:"text"` }
        if err := c.Bind(&in); err != nil { return nil, err }

        resp, err := c.LLM().Chat(c, []ai.Message{ /* your prompt */ })   // model proposes
        if err != nil { return nil, err }

        out := validate(resp.Content)   // your deterministic Go guardrail disposes
        return out, nil
    })
    app.Run()
}
```

That's it — you get tracing, token metrics, structured logs and a health check for free. To make it
reachable through the front door, register it in the **orchestrator** (see Part 2) and add a
Prometheus scrape target in `observability/prometheus.yml`. Copy the closest existing agent's module
as a starting point.

---

## Part 2 — How the agents work together

### The spine: every agent is a resilient service

The **orchestrator** is the front door. It registers each specialist as a GoFr HTTP *service* — which
means every agent-to-agent call gets a **circuit breaker, retries, a rate limiter and a health
check**, from config, with no handler code:

```go
app.AddHTTPService("sql-agent", envOr("SQL_AGENT_URL", "http://localhost:8007"),
    &service.CircuitBreakerConfig{Threshold: 4, Interval: 2 * time.Second},
    &service.RateLimiterConfig{Requests: 20, Window: time.Second, Burst: 25},
)
app.EnableAPIKeyAuth("agents-demo-key")   // front-door auth
```

### Routing: keyword-first, LLM as the tie-breaker

A request to `POST /assistant` is classified to one specialist. Classification is **deterministic
first** — a clear keyword wins immediately — and the LLM is only consulted for genuinely ambiguous
queries, with a keyword route as the fallback if the model is slow or answers oddly. So routing stays
correct even when the model is flaky. Adding a route is three small edits in `orchestrator/main.go`: a
`routes` entry (which service + path + how to shape the body), a line in the classifier prompt, and a
keyword case.

### Agents calling agents

Composition isn't limited to the orchestrator — **any agent can call any other** over the same
resilient services, and because the trace context propagates automatically, a multi-hop request is
still **one distributed trace**. The repo already does this:

```go
// memory-agent compacts old turns by calling summarizer-agent, over a circuit-broken service
resp, _ := c.GetHTTPService("summarizer-agent").Post(c, "summarize", nil, body)
```

That's the seam for building **pipelines**: `research-agent` → `summarizer-agent` → `scheduler-agent`
("research X, summarise it, and remind me tomorrow") is just three resilient calls, one trace.

### One trace, one dashboard, for all of them

Because every hop — the router's `llm.generate`, the inter-agent HTTP call, the specialist's
`llm.chat` and tool calls — runs on the same GoFr instrumentation, a single `/assistant` request is
**one Jaeger trace across services**, and every agent exports the same Prometheus metrics
(`app_llm_request_count`, HTTP throughput, latency) into the same Grafana dashboard. Point the whole
fleet at the stack with `TRACE_EXPORTER=otlp` + `TRACER_URL`.

### What's next (see the ROADMAP)

The composition story deepens with an **agent registry / discovery endpoint** (so the orchestrator
learns capabilities instead of hard-coding routes), **per-agent auth + rate-limiting**, and a
**gateway/dashboard UI** — turning this from "a router in front of specialists" into a self-hostable
multi-agent platform.

---

**TL;DR** — Customise by editing `.env` (provider, model, datasource, guardrail knobs); extend by
copying an agent module and implementing one handler. Compose by registering agents as resilient GoFr
services and letting them call each other — the orchestrator routes the front door, any agent can call
any other, and it all lands in one trace and one dashboard.
