# Roadmap

Where `agents` is headed: a self-hostable, fully observable **multi-agent platform** on
[GoFr](https://gofr.dev) — drop in a specialist agent, route between them, and get tracing, metrics,
auth, rate-limiting and resilience for free.

## Shipped

- **orchestrator** — LLM router over resilient HTTP services (circuit breaker · retry · rate limit · auth)
- **data-agent** — MCP agent loop over its own API
- **support-agent** — ticket triage + SSE streaming
- **kb-agent** — retrieval-augmented (RAG) helpdesk

## Planned agents

- [ ] **research-agent** — multi-source web research with citations
- [ ] **code-review-agent** — review a diff and comment inline
- [ ] **sql-agent** — natural language → SQL over a datasource
- [ ] **summarizer-agent** — long-document / thread summarization
- [ ] **pii-redaction-agent** — compliance / redaction
- [ ] **scheduler-agent** — plans and fires tasks

## Toward a product

- [ ] agent registry / discovery endpoint
- [ ] per-agent capability manifest + config
- [ ] auth + rate-limiting on every agent (not just the orchestrator)
- [ ] tests + CI (build all modules on every push)
- [ ] one-command `docker-compose` for the whole system
- [ ] a gateway / dashboard UI

## Changelog

- **2026-07-22** — initial release: orchestrator + data / support / kb agents, the local test shim,
  and the Jaeger + Prometheus + Grafana observability stack.
