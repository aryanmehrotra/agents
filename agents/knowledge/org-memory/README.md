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
# simplest: in-memory store + local (offline, deterministic) embedder — no infra, no keys
ORGMEM_EMBED=local go run .

# recommended local: embedded SQLite store + REAL semantic embeddings via Ollama
ollama serve & ollama pull nomic-embed-text
ORG_MEMORY_DB=./org-memory.db go run .        # EMBED defaults to http://localhost:11434/v1
```
Recall is a **GET**, so `app.EnableMCP()` exposes it as the `recall_decisions` MCP tool — point Cursor /
Claude Code at this service and it works. Writes are POST.

## Embeddings (real semantic recall)
By default it calls any **OpenAI-compatible `/v1/embeddings`** endpoint — set:
```
EMBED_BASE_URL=http://localhost:11434/v1   # Ollama (default) · your orchestrator · OpenAI
EMBED_MODEL=nomic-embed-text               # 768-dim, local, private
EMBED_API_KEY=...                          # only if the provider needs it
ORGMEM_EMBED=local                         # explicit offline mode (deterministic, no provider)
```
**No silent fallback:** if the provider is down it fails loudly rather than mixing mismatched vectors
into the store. (A Claude subscription gives *chat*, not embeddings — point EMBED at an embeddings model
like Ollama's `nomic-embed-text`.)

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

## Storage (behind the `Store` interface — swap anytime, all zero-infra)
- `memStore` — in-memory (default; data gone on restart)
- `fileStore` — JSON file, `ORG_MEMORY_PATH=./memory.json`
- `sqliteStore` — **embedded pure-Go SQLite** (no CGO, no server), `ORG_MEMORY_DB=./org-memory.db`
- at scale: a `sqlite-vec` index → Postgres+pgvector — same interface, engine unchanged.

Recall does **exact brute-force cosine** (correct at this scale; an ANN index is only worth it at
millions). Correctness of *what's surfaced* comes from the ranking + precision + (later) abstain layers,
not the store — the store just returns true nearest neighbours.

## Tests
```bash
go test ./...            # 22 tests
go test -race ./...      # + concurrency/consistency gate
```
Covers: dedupe, supersede-never-delete, scope match, precision floor / **inject-nothing**, ordering,
feedback nudge + auto-quarantine, fractal config, persistence-across-reload, and concurrent consistency.

## Not yet (by design — earn it with data)
The **correctness / abstain layer** (Phase 1), real-time relevance learning (Phase 2), the
associative/analogical retrieval, and domain packs (Phase 3). See the phased plan for what each later
phase enables and why the order is forced by the data each phase needs.
