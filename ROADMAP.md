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
- **pii-redaction-agent** — detect and redact PII, structured entities + risk score
- **summarizer-agent** — long-document / thread summarization, structured + streamed
- **memory-agent** — conversational agent with real long-term memory (stateless model + SurrealDB vector recall)
- **sql-agent** — natural language → SQL over a datasource, with a read-only guardrail on generated queries
- **research-agent** — multi-source web research with citations, SSRF-guarded outbound fetch

## Planned agents

- [ ] **scheduler-agent** — plans and fires tasks

## Toward a product

- [ ] agent registry / discovery endpoint
- [ ] per-agent capability manifest + config
- [ ] auth + rate-limiting on every agent (not just the orchestrator)
- [ ] tests + CI (build all modules on every push)
- [ ] one-command `docker-compose` for the whole system
- [ ] a gateway / dashboard UI

## Changelog

- **2026-07-26** — added **research-agent**: multi-source web research grounded in real, fetched
  page content, with inline `[n]` citations back to a numbered source list — the same
  citation-first, multi-source answer pattern behind Perplexity and ChatGPT Deep Research, one of
  the most visible AI-agent categories going into production this year, with enterprise agents
  increasingly used as research assistants that compile reports from external sources
  ([Forbes, "5 Amazing AI Agent Use Cases That Will Transform Any Business In
  2026"](https://www.forbes.com/sites/bernardmarr/2025/11/25/5-amazing-ai-agent-use-cases-that-will-transform-any-business-in-2026/);
  [EasyClaw, "Best AI Research Agents in
  2026"](https://easyclaw.com/blog/top-lists/best-ai-research-agents/)). Because the URLs it fetches
  come from untrusted, user-supplied text, every URL is checked by a deterministic Go guardrail
  before any outbound request — blocking non-http(s) schemes, embedded credentials, and
  localhost/internal/metadata/private/loopback/link-local hosts (the classic `169.254.169.254`
  cloud-metadata SSRF target) — re-checked on every redirect hop. Wired into the orchestrator's new
  `research` route, with a keyword fallback that catches any query containing a link.
- **2026-07-25** — added **sql-agent**: natural-language question → LLM-generated SQL → executed
  against a zero-config `c.SQL` datasource (SQLite locally; swap `DB_DIALECT` for a real warehouse),
  answered grounded in the actual result rows — with a read-only-SELECT guardrail in front of
  execution, since the generated SQL runs directly against the database. Wired into the
  orchestrator's new `sql` route. Text-to-SQL / natural-language database querying is one of the
  clearest production "BI agent" patterns right now — translating natural language into SQL,
  executing it, and returning results to reduce manual query writing, alongside dashboard
  generation and ad-hoc data exploration ([ema.co, "Building AI Agents for Databases with SQL and
  Natural Language"](https://www.ema.co/additional-blogs/addition-blogs/building-ai-agents-sql-natural-language)).
- **2026-07-24** — added **memory-agent**: a conversational agent with real long-term memory over a
  **stateless** chat model — working memory (recent turns) plus long-term recall by vector similarity
  in SurrealDB via GoFr's `Embedder` capability, with old turns compacted through the
  summarizer-agent once a session grows. (Recorded here for completeness — it shipped same-day as
  summarizer-agent below but was missed in this changelog at the time.)
- **2026-07-24** — added **summarizer-agent**: structured breakdown (tl;dr, key points, action
  items, decisions, open questions) plus a streamed narrative summary, wired into the
  orchestrator's new `summarize` route. Summarization keeps showing up as one of the most
  common tightly-scoped sub-agent responsibilities behind a supervisor/router layer, alongside
  retrieval and classification, as teams move from single do-everything agents to composed
  specialist agents in production ([MLflow, "Building Production-Ready AI Agents in
  2026"](https://mlflow.org/articles/building-production-ready-ai-agents-in-2026/)).
- **2026-07-23** — added **pii-redaction-agent**: LLM-detected PII entities (name, email, phone, SSN,
  credit card, address, DOB, IP) redacted deterministically in Go, plus a streamed compliance
  rationale; wired into the orchestrator's new `redact` route. Compliance/redaction agents are
  a top enterprise use case going into production now — BFSI compliance monitoring is cited
  alongside customer service and code review as a leading 2026 deployment, and industry-specific
  compliance stacks (BFSI, legal, healthcare) are consolidating fast ([HyScaler, "12 Enterprise AI
  Agents Use Cases Transforming Enterprises in 2026"](https://hyscaler.com/insights/enterprise-ai-agents-use-cases/)).
- **2026-07-22** — added **code-review-agent**: structured, file/line-anchored diff review + streamed
  prose review, wired into the orchestrator's `review` route. AI code review is one of the most
  commercially proven agent use cases right now — AI now generates 41% of code globally and GitHub
  Copilot serves 20M users across 90% of the Fortune 100 ([HyScaler, "12 Enterprise AI Agents Use
  Cases Transforming Enterprises in 2026"](https://hyscaler.com/insights/enterprise-ai-agents-use-cases/)).
- **2026-07-22** — initial release: orchestrator + data / support / kb agents, the local test shim,
  and the Jaeger + Prometheus + Grafana observability stack.
