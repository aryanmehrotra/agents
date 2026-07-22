# Roadmap

Where `agents` is headed: a self-hostable, fully observable **multi-agent platform** on
[GoFr](https://gofr.dev) — drop in a specialist agent, route between them, and get tracing, metrics,
auth, rate-limiting and resilience for free.

## Shipped

- **orchestrator** — LLM router over resilient HTTP services (circuit breaker · retry · rate limit · auth)
- **data-agent** — MCP agent loop over its own API
- **support-agent** — ticket triage + SSE streaming
- **kb-agent** — retrieval-augmented (RAG) helpdesk
- **code-review-agent** — review a diff and comment inline

## Planned agents

- [ ] **research-agent** — multi-source web research with citations
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

- **2026-07-22** — added **code-review-agent**: structured, file/line-anchored diff review + streamed
  prose review, wired into the orchestrator's `review` route. AI code review is one of the most
  commercially proven agent use cases right now — AI now generates 41% of code globally and GitHub
  Copilot serves 20M users across 90% of the Fortune 100 ([HyScaler, "12 Enterprise AI Agents Use
  Cases Transforming Enterprises in 2026"](https://hyscaler.com/insights/enterprise-ai-agents-use-cases/)).
- **2026-07-22** — initial release: orchestrator + data / support / kb agents, the local test shim,
  and the Jaeger + Prometheus + Grafana observability stack.
