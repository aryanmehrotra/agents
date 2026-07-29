# org-memory (Phase 0)

The generic **org-memory engine**: it captures the *why* behind decisions, surfaces the few that
apply to what you're doing — to an AI agent or to you — and **learns from your feedback**. Advisory
only: it never decides truth on its own. Runs **locally, offline, zero-config**.

```
capture ──▶ store ──▶ recall ──▶ advise
                          ▲            │
                          └── feedback ┘   (helpful / not_relevant / wrong → it adapts)
```

Everything is behind swappable interfaces (`Store`, `Embedder`, `Config`) and every behaviour is a
**live config knob — nothing is hardcoded**. `supersede/quarantine, never delete`.

## Run
```bash
# in-memory (data gone on restart)
go run .

# persistent (JSON file, survives restarts)
ORG_MEMORY_PATH=./memory.json go run .
```
Recall is a **GET**, so `app.EnableMCP()` exposes it as the `recall_decisions` MCP tool — point Cursor /
Claude Code at this service and it works. Writes are POST.

## Endpoints
| Method | Path | What |
|---|---|---|
| GET | `/recall?context=repo:x,service:pay&team=pay` | the 2–3 relevant prior decisions, or nothing |
| POST | `/capture` | record/reinforce a decision |
| POST | `/feedback` | ★ the improvement loop |
| GET/POST | `/config` | read / change a knob live |

```bash
# capture
curl -XPOST :8000/capture -d '{"what":"money is int64 cents, never float","why":"float rounding caused an incident","scope":["service:pay"],"provenance":"senior"}'

# recall (what an agent calls)
curl ':8000/recall?context=service:pay,handling%20refund%20money'

# feedback — improve it
curl -XPOST :8000/feedback -d '{"decision_id":"d-xxxx","signal":"helpful"}'
curl -XPOST :8000/feedback -d '{"decision_id":"d-xxxx","signal":"wrong"}'   # 2x wrong → auto-quarantined

# tune any knob live (no redeploy)
curl -XPOST :8000/config -d '{"key":"retrieve.precision_floor","value":"0.5"}'
```

## Feedback signals
`helpful` / `used` → boosts it · `not_relevant` → demotes · `wrong`/`outdated` → after N (config
`feedback.wrong_quarantine_at`, default 2) it's **auto-quarantined** (kept, not deleted). Feedback is a
*relevance* signal only — never treated as correctness (that's a later phase).

## Config knobs (all live, fractal org→team→person)
`retrieve.precision_floor` (0.30) · `retrieve.top_k` (3) · `rank.w_relevance/recency/importance` ·
`feedback.boost_per_helpful` (0.1) · `feedback.demote_per_notrel` (0.1) · `feedback.wrong_quarantine_at` (2).
Set at deploy via `ORGMEM_<key>` env (e.g. `ORGMEM_retrieve__precision_floor=0.4`) or live via `POST /config`.

## Storage (behind the `Store` interface — swap anytime)
- **local / Phase 0**: in-memory or the JSON `fileStore` (this module) — zero ops.
- **at scale**: `sqlite-vec` (embedded) → Postgres+pgvector. The engine never changes.
Correctness comes from the ranking + precision + (later) abstain layers, not the store — the store just
returns true nearest neighbours (exact brute-force cosine here, correct at this scale).

## Tests
```bash
go test ./...            # 22 tests
go test -race ./...      # + concurrency/consistency gate
```
Covers: dedupe, supersede-never-delete, scope match, precision floor / **inject-nothing**, ordering,
feedback nudge + auto-quarantine, fractal config, persistence-across-reload, and concurrent consistency.

## Not in Phase 0 (by design — earn it with data)
Correctness/abstain layer, real-time relevance learning, the associative/analogical retrieval, domain
packs, an LLM-backed embedder (the default local embedder is deterministic bag-of-words — swap via the
`Embedder` interface). See the phased plan for what each later phase enables and why.
