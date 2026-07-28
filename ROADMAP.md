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
- **extraction-agent** — unstructured text → structured, typed JSON against a caller-declared schema, validated deterministically in Go
- **local-rag-agent** — 100% on-device RAG: llama.cpp in-process (Kronk) for embeddings + chat, SurrealDB vectors, exposed through GoFr as a custom `ai.Model`
- **scheduler-agent** — plans and fires tasks: natural language → scheduled webhook, with an SSRF guardrail on the outbound URL at both schedule- and fire-time

## Planned agents

_(none currently — see "Toward a product" for what's next)_

## Toward a product

- [ ] agent registry / discovery endpoint
- [ ] per-agent capability manifest + config
- [ ] auth + rate-limiting on every agent (not just the orchestrator)
- [ ] tests + CI (build all modules on every push)
- [ ] one-command `docker-compose` for the whole system
- [ ] a gateway / dashboard UI

## Changelog

- **2026-07-28** — added **scheduler-agent**: turns a natural-language request ("in 10 minutes, ping
  my webhook to check the deploy") into a scheduled task and actually fires it — a background ticker
  goroutine fires due tasks independent of any single HTTP request. Task-planning/scheduling agents
  that track deadlines and fire reminders or follow-up actions without a human re-triggering them are
  one of the clearest "always-on" agent use cases going into production now, part of the broader shift
  from single-shot chat toward agents that plan and execute multi-step work over time ([Sema4.ai, "10
  AI Agent Use Cases Transforming Enterprises in
  2026"](https://sema4.ai/blog/ai-agent-use-cases/); Gartner: 40% of enterprise applications will
  integrate task-specific AI agents by end of 2026, cited via
  [Lindy](https://www.lindy.ai/blog/ai-agents-examples)). Because firing a task means making a real
  outbound HTTP request to a webhook URL that came out of untrusted, model-parsed text, every URL goes
  through the same deterministic SSRF guardrail `research-agent` uses — refused at scheduling time
  (never even queued) and re-checked immediately before it fires. Wired into the orchestrator's new
  `schedule` route, with a keyword fallback for remind/reminder/schedule/webhook/cron requests.
- **2026-07-27** — added **extraction-agent**: turns unstructured text into structured, typed JSON
  against a caller-declared schema (each field a name + type — `string`, `integer`, `number`,
  `boolean`, `date`, or a `<type>[]` list). Structured extraction / "structured output" — parsing
  invoices, resumes, contracts and entities out of free-form text into a fixed schema — is the single
  most widely deployed LLM pattern in production today, the backbone of document processing,
  "chat with your data" and unstructured-to-structured ETL ([K2view, "Top AI agent use cases in the
  enterprise (2026)"](https://www.k2view.com/blog/ai-agent-use-cases/)). Because a model reliably
  *finds* values but cannot be trusted to *shape* them — it invents keys, returns the wrong type, or
  wraps its JSON in prose — the model only proposes: every value it returns is validated
  deterministically in Go against the requested schema (keys not in the schema dropped, values that
  don't match their declared type rejected, required-but-unresolved fields reported missing) so the
  caller gets typed data it can rely on, never the model's raw guess. Wired into the orchestrator's
  new `extract` route, with a keyword fallback for parse/extract/structured queries.
- **2026-07-27** — added **local-rag-agent**: a fully on-device retrieval-augmented-generation agent.
  Both the embedding model and the chat model run **in-process on llama.cpp** via
  [Kronk](https://github.com/ardanlabs/kronk) (GGUF, no daemon, no HTTP, no API key), and document
  vectors are stored in SurrealDB — used as a vector database through GoFr's datasource. Ingested
  text is chunked, embedded and stored; a question is embedded, matched by cosine similarity, and
  answered by the local model grounded only in the retrieved passages, with `[n]` citations. The
  novelty is on the GoFr side: the local engine is exposed as a **custom `ai.Model`** (registered
  with `app.AddLLM`), so `ctx.LLM()` drives on-device inference while GoFr still supplies tracing,
  token metrics (`app_llm_request_count{provider="kronk"}`) and a health check — the same
  batteries every hosted-provider agent here gets, over an engine that isn't an HTTP API at all.
  Local/private inference is a fast-growing production requirement where data residency, cost or
  air-gapping rule out hosted APIs. Config-driven and generic: point `CHAT_MODEL` / `EMBED_MODEL` at
  any GGUF Kronk can resolve (see the agent README's "Customising" section). Wired into the
  orchestrator's new `localdocs` route.
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
