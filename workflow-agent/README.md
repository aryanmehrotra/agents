# workflow-agent

Turns one high-level **goal** into a multi-step **workflow across the whole fleet** — without knowing
which agents exist. Give it a goal; its LLM breaks it into ordered sub-tasks; it dispatches each one
to the **orchestrator hub** (the same `/assistant` route that routes any query to the right
specialist), threading each step's output into the next, and synthesises a final answer.

```
POST /workflow {"goal": "Summarise this incident, then extract the service and severity as fields …"}

plan  → [ "summarise the incident …", "extract the affected service and severity …" ]
step 1 → orchestrator → summarizer-agent  → "checkout-api returned 500s for ~20 min during peak …"
step 2 → orchestrator → extraction-agent  → { affected_service, severity }
→ result: "checkout-api returned 500s for ~20 min during peak, high-severity page.
           affected_service: checkout-api · severity: high"
```

## Why this stays generic

The workflow-agent hard-codes **no peer**. It knows exactly one address — the orchestrator
(`ORCHESTRATOR_URL`) — and the orchestrator already knows how to reach every specialist. So:

- **Add or remove agents** and existing workflows keep working — routing is the orchestrator's job.
- **No per-pair glue.** A step is just a natural-language sub-task; the hub decides who handles it.
- **The model proposes, Go disposes.** The plan must be a JSON array of `1..6` non-empty tasks or it's
  refused before any step runs — never silently truncated.
- **One step failing doesn't abort the workflow** — it's recorded with its error and the rest continue.
- Every hop is a resilient GoFr HTTP call, so a whole workflow is **one distributed trace**.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/workflow` | `{goal}` → `{goal, steps[], result}` — `steps` lists each planned task, which specialist the hub routed it to, and its output; `result` is the synthesised answer |

## Run

It sits *above* the fleet, so it needs the orchestrator (and the specialists a goal will touch)
running. From the repo root, keyless via the shim:

```bash
# shim + specialists + orchestrator (see the top-level README quickstart), then:
cp configs/.env.local configs/.env && go run .
```

## Try it

```bash
curl -s localhost:8012/workflow -H 'Content-Type: application/json' -d '{
  "goal": "Summarize this incident in one sentence, then extract the affected service and severity as fields. Incident: the checkout-api returned HTTP 500 for ~20 minutes during peak; on-call paged; severity high; service checkout-api."
}'
# → { "steps": [
#      {"step":1,"routed_to":"summarizer-agent","output":"…"},
#      {"step":2,"routed_to":"extraction-agent","output":"…"} ],
#    "result":"…affected_service: checkout-api · severity: high" }
```

Pipelines fall out naturally: *"research https://… and summarise it in three bullets"* →
`research-agent` → `summarizer-agent`; *"find the top sales rep in the database, then draft a reminder
to congratulate them"* → `sql-agent` → `scheduler-agent`.

## Config

| Variable | Default | What it does |
|----------|---------|--------------|
| `ORCHESTRATOR_URL` | `http://localhost:8080` | The hub every step is dispatched through |
| `API_KEY` | `agents-demo-key` | The orchestrator's front-door API key, sent on each dispatch |
| `LLM_PROVIDER` / `LLM_MODEL` / … | — | Planner + synthesiser model (any provider — see the repo GUIDE) |

`main_test.go` covers the deterministic parts: plan parsing + the `1..6`-step guardrail, unwrapping
the orchestrator's response envelope, output extraction, and the bounded cross-step context threading.
