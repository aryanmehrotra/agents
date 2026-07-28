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
- **workflow-agent** — turns one goal into a multi-step workflow across the fleet: plans ordered sub-tasks and dispatches each through the orchestrator hub, threading outputs — composing any agents without hard-coding a single peer
- **spec-agent** — turns a raw ticket/issue into a structured engineering spec (scope, testable acceptance criteria, risks, task breakdown), normalized deterministically in Go and gated on a real spec before it's called complete
- **estimation-agent** — sizes a task breakdown into a point estimate with an optimistic/likely/pessimistic range (and a duration, given velocity); the model only picks relative sizes and confidence, and every number is computed in Go from a fixed size→points table — the model's own arithmetic is never trusted
- **scaffold-agent** — generates a runnable project skeleton from a spec **in any stack** (Python, Node, Go, Rust, …); the model proposes files and Go disposes — every path is sanitized against traversal/escape, binary/non-UTF-8 files are rejected, and files it can parse (Go/JSON/YAML) are syntax-checked, all in-process (no disk writes, no repo touched)
- **migration-agent** — applies a mechanical codemod across a set of files **in any language**; the model rewrites and Go disposes — a deterministic per-file diff, and for the types it can parse (Go/JSON/YAML) the rewrite is re-verified so a change that no longer parses is rejected and the original kept, all in-process (no repo touched)
- **test-gen-agent** — writes unit tests for a piece of code and, for Go, **compiles and runs them** in an isolated offline temp module — the test is only "kept" if it built and passed, so a green result is one that was actually executed, not just generated (other languages are generated but marked not-executed)
- **flaky-test-agent** — mines CI run history for flaky tests; detection is **deterministic Go** (a test is flaky iff it both passed and failed across the runs), ranked by fail rate with a quarantine list, and always-failing tests are separated out as broken-not-flaky — the model only annotates a likely cause, and a model outage loses only the annotations

## Planned agents — the software development lifecycle

Next up: a suite that points the same spine (orchestrator + resilient HTTP + `ctx.LLM()` + built-in
observability) at the work of **shipping software** — an agent for each stage of the SDLC. Every one
keeps the repo's rule: the model *proposes*, a deterministic Go guardrail *disposes*, and the outcome
is verified (it compiles, the tests pass, the check is real) before anything is trusted.
(`code-review-agent` already covers the review step.)

**Plan & design**
- [x] **spec-agent** — turn a ticket/issue into a structured spec (scope, acceptance criteria, risks) plus a task breakdown ✅ *shipped*
- [x] **estimation-agent** — size the work from that spec and the repo's own history ✅ *shipped*

**Build**
- [x] **scaffold-agent** — generate a service/module skeleton (handlers, config, tests) from a spec ✅ *shipped* (returns verified files in-process; opening a PR is a caller's choice)
- [x] **migration-agent** — apply a mechanical codemod across files and verify it still parses ✅ *shipped* (operates on supplied file contents in-memory; a broken rewrite is rejected)

**Test**
- [x] **test-gen-agent** — write/maintain unit tests for changed code, gated on "must compile and pass" before it's kept ✅ *shipped*
- [x] **flaky-test-agent** — mine CI history for flaky tests and quarantine/report them ✅ *shipped*

**Review & release**
- [ ] **breaking-change-agent** — detect API/contract breaks in a diff before merge
- [ ] **release-notes-agent** — draft a changelog / release notes from the merged PRs in a range
- [ ] **dependency-agent** — propose and validate dependency bumps (surfaced only if build + tests stay green) — the pattern behind this repo's own daily dependency PRs

**Operate**
- [ ] **incident-triage-agent** — triage an alert or stack trace to a likely root cause and owner, grounded in logs and traces
- [ ] **oncall-summary-agent** — distil an incident channel into a timeline and action items

## Toward a product

- [x] agent registry / discovery endpoint ✅ *shipped* (`/capabilities`, registry-driven routing)
- [~] per-agent capability manifest + config — *partial:* a central capability registry drives routing + discovery today; agents self-serving their own manifest is the next step
- [ ] auth + rate-limiting on every agent (not just the orchestrator)
- [ ] tests + CI (build all modules on every push)
- [ ] one-command `docker-compose` for the whole system
- [ ] a gateway / dashboard UI

## Changelog

- **2026-07-28** — added **flaky-test-agent**: mines CI run history for flaky tests. It deliberately
  inverts the usual "model proposes, Go disposes" — here the *detection* is the deterministic part and
  lives entirely in Go: a test is flaky **iff**, in the runs you provide, it has at least one pass AND
  at least one fail, so the model never gets to decide what's flaky. Go tallies each test, ranks the
  flaky ones by fail rate (with a flip count for how erratic they are), recommends quarantine, and
  keeps **always-failing tests separate** — those are broken, not flaky, and shouldn't be quarantined.
  The model's role is narrow and advisory: given the failure messages it annotates each
  already-detected flaky test with a likely-cause category (timing/race, external-dependency, …) and a
  one-line fix. If the model is unavailable, the detection, ranking and quarantine list still stand —
  you just lose the annotations. Verified live on a 13-row history (TestSearch 0.67 and TestLogin 0.40
  flagged flaky + quarantine, TestPayment separated as always-failing). Added as a single
  capability-registry entry — no routing changes. Wired into the orchestrator's `flaky` route.
- **2026-07-28** — added **test-gen-agent**: the test stage of the SDLC suite — it writes unit tests
  for a piece of code and, crucially, **doesn't trust what the model wrote**. For Go it writes the
  generated test next to the source in a throwaway module and actually runs `go test` — fully offline
  (`GOPROXY=off`, a hard timeout, no toolchain download, so the agent can't hang or reach the network)
  — and reports whether it *built* and *passed*. A test that fails to compile or fails to pass is
  returned with its output and marked **not kept**: you see exactly why, and you never get a green
  result you can't trust (a test that doesn't compile, or passes vacuously, is worse than none). Other
  languages get a generated test too, honestly marked "not executed" (only Go is run here). Nothing is
  written to your repo; the temp module is created and removed per request. This is the first agent
  added since the router rework — it plugged in as a **single capability-registry entry, with no
  routing changes at all**. Wired into the orchestrator's `testgen` route.
- **2026-07-28** — reworked **orchestrator routing** to be **registry-driven and LLM-first**. Routing
  used to run a hand-maintained keyword chain *first* and consult the model only for leftovers, over a
  hardcoded per-agent prompt — brittle, and every new agent meant editing both. Now a single
  **capability registry** (route, service, request shape, description, fallback keywords per agent) is
  the one source of truth: it drives the resilient-service registration, a router prompt **generated
  from the live descriptions** (so the model routes over the current agent set with nothing to keep in
  sync by hand), and a new **`GET /capabilities`** discovery endpoint. The model is now the primary
  router; a registry-derived keyword match is only a fallback for when the model call fails. Verified
  live: keyword-free queries ("shrink this wall of text into three bullet points", "lay out scope,
  risks and work items") route correctly to summarizer / spec — the keyword fallback would have sent
  both to the default agent, so the model is demonstrably doing the routing. Adding an agent is now one
  registry entry — no keyword chains or prompt prose to edit. Lands the roadmap's *agent registry /
  discovery endpoint*.
- **2026-07-28** — added **migration-agent**: the maintenance side of the build stage — it applies a
  mechanical codemod (rename, replace a deprecated API, add a header) across a set of files **in any
  language**, and verifies it didn't break them. A model is good at *proposing* the edit but can't be
  trusted that the result still holds together — it drops a brace, mangles a string, or rewrites more
  than you asked. So the model only proposes new file content and Go disposes: it computes a
  deterministic per-file diff (an LCS line diff, so a model that quietly touched an unrelated line is
  visible), and for the file types it can parse — Go via `go/format.Source`, JSON, YAML — it re-parses
  the rewrite and, if the codemod produced something that no longer parses, **rejects the change and
  keeps the original**. A migration must never leave a file worse than it found it. It's all
  in-process (no disk writes, no repo touched); you get the rewritten content and diffs to apply
  yourself. Wired into the orchestrator's new `migrate` route, with a keyword fallback for
  codemod / rename-across / replace-deprecated requests.
- **2026-07-28** — added **scaffold-agent**: the build stage of the SDLC suite — it turns a spec into
  a runnable project skeleton **in any language or framework** (Python/FastAPI, Node/Express, Go/GoFr,
  Rust, … — name the stack or let the model infer it), the repetitive first hour of a new service done
  in seconds. Generated files are only worth anything if they're safe and real, so the model only
  *proposes* files and Go *disposes*, all language-agnostic: every path is sanitized (absolute paths,
  `..`, anything escaping the scaffold root, and binary/executable types are rejected — a filesystem
  analogue of the SSRF guardrail the fetch agents use) and every file must be valid UTF-8 text; then
  the files it *can* parse are syntax-checked — Go via `go/format.Source` (returned gofmt'd), JSON via
  `encoding/json`, YAML via `yaml.v3` — while a file in a language it can't parse is returned untouched
  and honestly marked *unchecked*, never silently blessed. It's all in-process: no disk writes, no
  build, no network, so the agent never touches your repo and never blocks. You get the files plus an
  honest list of what was rejected, to write out yourself (or hand to a PR step). Wired into the
  orchestrator's new `scaffold` route, with a keyword fallback for scaffold / skeleton / boilerplate /
  starter-code requests.
- **2026-07-28** — added **estimation-agent**: the second stop in the SDLC suite — it sizes a task
  breakdown (for example spec-agent's output) or a raw description into a point estimate with an
  optimistic/likely/pessimistic range, and — given a team velocity — a duration in working days. The
  division of labour is the whole design: a model is genuinely useful at the *judgment* (sizing one
  task relative to another, and saying how confident it is) but must not be trusted with the
  *arithmetic* — models miscount, drop tasks from a sum, and confidently return a total that doesn't
  match their own per-task sizes. So the model only proposes a T-shirt size and a confidence per task;
  every number after that is computed deterministically in Go from a fixed size→points ladder, the
  per-task confidence bands are summed into the range (so it reflects *which* tasks are uncertain, not
  a blanket fudge factor), and any total the model volunteers is recorded next to the one Go computed
  and then ignored. Wired into the orchestrator's new `estimate` route, with a keyword fallback for
  estimate / story-points / how-long / sizing requests.
- **2026-07-28** — added **spec-agent**: the first stop in the SDLC suite — it turns a raw ticket or
  issue into a structured engineering spec: a one-line summary, what's in and out of scope, testable
  acceptance criteria, risks, a task breakdown, and the open questions that still block the work. This
  is the stage where an LLM most reliably saves a human time (read the ticket, draft the shape, a
  person edits) — but a spec is only useful if it's actually a spec, and a model asked for one will
  happily return prose, leave the acceptance criteria empty, or bury the tasks in a sentence. So the
  model only *proposes*: every section is normalized deterministically in Go — blanks and duplicates
  dropped, every task required to carry a title, lists capped — and the result is *gated* on a hard
  minimum (a real summary, at least one acceptance criterion, at least one task) before it's called
  complete, with an honest `incomplete_reasons` note about whatever the model failed to fill in. Wired
  into the orchestrator's new `spec` route, with a keyword fallback for spec / acceptance-criteria /
  task-breakdown / user-story requests.
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
- **2026-07-28** — added **workflow-agent**: the composition layer. It turns one high-level goal into
  a multi-step workflow across the whole fleet — its LLM breaks the goal into ordered sub-tasks, and it
  dispatches each one through the **orchestrator hub** (the same `/assistant` route that routes any
  query to the right specialist), threading each step's output into the next and synthesising a final
  answer. The point is that it stays generic: it hard-codes **no peer** — it knows only the orchestrator
  (`ORCHESTRATOR_URL`), and the orchestrator already knows how to reach every agent, so add/remove
  agents and existing workflows keep working. The model *proposes* the plan; a deterministic Go
  guardrail *disposes* — a plan must be a JSON array of 1..6 non-empty tasks or it's refused before any
  step runs, one failing step doesn't abort the workflow, and cross-step context is bounded. Every hop
  is a resilient GoFr HTTP call, so a whole workflow is one distributed trace. Composing agents into
  plan-and-execute pipelines is the shift from single-shot chat to agents that carry out multi-step
  work — "research this link and summarise it," "find the top rep and schedule a note" become
  research→summarize and sql→schedule with no per-pair glue.
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
