# Observability stack

GoFr emits traces, metrics and logs for every LLM and tool call with **no extra code**. This folder
spins up a local stack to *see* them:

- **Jaeger** (traces) — http://localhost:16686
- **Prometheus** (metrics) — http://localhost:9090
- **Grafana** (dashboards) — http://localhost:3000 *(anonymous, dark, LLM dashboard pre-provisioned)*

## Run

```bash
# 1 · start the stack
docker compose -f observability/docker-compose.yml up -d      # or: docker-compose -f ...

# 2 · run an agent with tracing on (metrics are always on)
cd data-agent
cp configs/.env.local configs/.env
cat >> configs/.env <<'EOF'
TRACE_EXPORTER=otlp
TRACER_HOST=localhost
TRACER_PORT=4317
EOF
go run .

# 3 · generate some traffic
curl -s localhost:8000/ask -d '{"question":"Which products are out of stock and what is our shipped revenue?"}'
```

Then open:
- **Grafana** → *GoFr Agents — LLM Observability* dashboard: token usage (prompt/completion), request counts.
- **Jaeger** → service `data-agent` → one `/ask` request expands into `llm.chat` spans and the tool
  calls it made, all under a single trace.

Metrics come from each agent's `METRICS_PORT` (data-agent `2121`, support `2122`, kb `2123`, …,
memory-agent `2128`), scraped by Prometheus via `host.docker.internal`.

**Embeddings are traced and metered like chat.** The [`memory-agent`](../memory-agent) embeds text
for vector recall through GoFr's `Embedder`, so its `llm.embed` spans appear alongside `llm.chat` in
one `POST /chat` trace, the LLM dashboard's *requests by operation* panel shows `embed` beside `chat`,
and the bottom row charts the memory payoff — **bounded vs naive context tokens** and the
**cumulative tokens saved** by recall (custom `app_memory_*` metrics the agent emits):

![memory-agent trace](../docs/jaeger-memory-trace.png)
![embed vs chat in Grafana](../docs/grafana-memory-embed.png)

<sub>The *inter-agent* panels show memory-agent → summarizer-agent compaction calls.</sub>

## Tear down

```bash
docker compose -f observability/docker-compose.yml down
```
