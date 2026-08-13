# Contradiction-Rate Validation — Decision Memory @ localhost:8000

**Date:** 2026-07-30
**Corpus:** 1,643 decisions (acme repo + books + gofr howtos + maintainer reviews)
**Endpoint:** `GET /recall?q=<query>` → top-k (k≈3) surfaced decisions
**Queries run:** 32 realistic engineering questions spanning the corpus's main themes
(cost/metrics storage, auth/gateway, ClickHouse reads, retries, caching, Redis usage,
deploy queue, scheduler/executor, overrides, ownership boundaries, tenancy).

---

## Headline

- **Strict contradiction rate: 3 / 32 = 9.4%** (queries surfacing ≥1 genuinely conflicting pair)
- **Inclusive rate (adding surface-level wording tensions reconciled by scope): 6 / 32 = 18.8%**
- **Superseded-alongside-replacement events: 0** — ~~but only because the memory has **no
  supersede mechanism at all**~~. **CORRECTED:** the mechanism exists and is honoured; superseded
  decisions are hidden from recall and from `/decisions`. 0 is the correct and meaningful result
  here. See the corrected root-cause section below.

All 3 true contradictions stem from a **single unresolved cluster**: how/where cost & metric
data is stored (MySQL vs ClickHouse, dual-write vs sole-store).

---

## The real contradiction: ClickHouse / MySQL order-storage

The corpus captured three storage decisions from three different source documents as **co-equal live
decisions**, and `recall` returns them side by side.

> ~~There is no `supersedes` / `superseded_by` field in the decision schema (verified against raw
> `/decisions` records — the only status flag is `quarantined`, all `false`).~~
> **CORRECTED 2026-07-30:** both fields exist and are honoured. Further, only *one* of the three
> decisions below is actually stale — two of them are true statements about different tables. See
> the corrected root-cause section for the source-document evidence.

Surfaced together, same query, directly opposite:

| id | `what` |
|----|--------|
| `d-cbfde40003b1` | "**ClickHouse is the sole store for orders — no MySQL dual-write**" |
| `d-05d7cb7f3b50` | "**Dual-write cost/allocation/metrics data to both MySQL and ClickHouse**, with ClickHouse as the read path gated behind WAREHOUSE_READS_ENABLED" |

These assert opposite things about the *same* subject (orders): "no MySQL dual-write"
vs "dual-write cost … to both MySQL and ClickHouse." A related third snapshot deepens the
inconsistency for metrics:

| id | `what` |
|----|--------|
| `d-cd39778fbe0c` | "**MySQL `order_events` is the source of truth**; the three ClickHouse tables are best-effort dual-write replicas" |

`cd39` (MySQL authoritative) vs `cbfd` (ClickHouse sole store) vs `05d7` (dual-write, CH read
path) cannot all be current. None is marked stale.

### Queries that surfaced this contradictory pair
1. **"is ClickHouse the source of truth for metrics and cost"** → returned `cbfd` + `cd39` + `05d7`
   → contradictory (sole-store vs MySQL-source-of-truth vs dual-write).
2. **"where do cost records live MySQL or ClickHouse"** → returned `cbfd` + `05d7` + `d-23df95c9ec53`
   → contradictory (`cbfd` "no MySQL dual-write" vs `05d7` "dual-write … to both MySQL and ClickHouse").
3. **"is dual-write between MySQL and ClickHouse used"** → returned `05d7` + `cbfd` + `d-2695eff0e38b`
   → contradictory (`05d7` "dual-write to both" vs `cbfd` "no MySQL dual-write").

**Impact:** an engineer asking the single most natural order-storage question gets both
"dual-write to MySQL and ClickHouse" and "ClickHouse only, no MySQL dual-write" in the same
top-3, with no signal about which is current. This is the exact danger the eval targets.

---

## Borderline tensions (NOT counted as true contradictions — reconciled by scope/layer)

These read as opposite in wording but are simultaneously true because they apply to different
layers or endpoint classes. Flagged because a naive reader could still be confused.

**A. Start/stop state pre-check** (queries: "are start stop actions idempotent…",
"how are start stop conflicting transitional states handled"):
- `d-bb98a09c0a73` — "Start/stop actions are idempotent and retried **without a state pre-check**"
- `d-b0d90c9dbb2b` (service:executor) — "Make start/stop idempotent **with a pre-flight state check**…"
- Reconciled by `d-bd0fa301ca54`, which was also surfaced: the **scheduler** dispatches intents
  without pre-checking; the **executor/provider** does the pre-flight check. Compatible layers,
  not a true contradiction — but the bare `what` strings say "without" vs "with a state pre-check."

**B. Auth location** (query: "where is authentication enforced gateway or handler"):
- `d-14e1c0b1c3b7` — "The gateway is the **sole authentication point** … no component trusts an
  unauthenticated request"
- `d-d40400f2e6c7` (service:deployer) — "Authenticate unauthenticated public endpoints **at the
  handler, not the gateway** — HMAC-signed JWT / HMAC-verified GitHub webhooks"
- Reconciled by endpoint class: gateway is sole auth for the customer JWT surface; specific public
  webhook/build-callback endpoints (which the gateway intentionally skips) are HMAC-authenticated at
  the handler. Compatible, not dangerous.

---

## Where ranking/consistency worked well (29/32 clean)

- **Budget ownership** ("who owns budget data") surfaced only the current config-owns-budgets
  decisions; the transitional `BUDGET_SOURCE` dual-mode migration decision did **not** appear.
- **Overrides** ("write-time vs read-time", "expire eagerly?") returned a consistent read-time-derivation
  set; the explicitly-interim write-time cascade decision did **not** surface.
- **Rate limiting fail-open** returned acme ("fails open when Redis errors") plus the book guidance
  ("fail open when store unavailable" / "decide per path") — consistent, mutually reinforcing.
- **Gateway = thin proxy, no business logic**, **components don't share DBs**, **recommender never reads
  metrics DB directly**, **ClickHouse ReplacingMergeTree + FINAL**, **admin-plane vs customer auth
  separation**, **per-resource discovery events**, **tenancy/orgID injection** — all returned internally
  consistent top-k sets.

That the budget-migration and override-cascade interim decisions stayed *out* of results is
encouraging — but it appears driven by embedding ranking, not by an explicit supersede/version
mechanism (which does not exist in the schema). The order-storage cluster shows ranking alone is
not sufficient: when the stale and current snapshots are equally on-topic, both surface.

---

## Verdict

The memory is **largely internally consistent (≈90% of queries clean)**, but it is **not safe on
its highest-traffic data-architecture question.** It carries one genuine, dangerous contradiction
cluster — cost/metric storage (dual-write vs ClickHouse-sole vs MySQL-source-of-truth) — and
surfaces the conflicting snapshots together for the three most natural order-storage queries.

> **CORRECTED 2026-07-30** (META_AUDIT correction #1, then corrected again against the source docs).
> Both the original root cause and the audit's replacement for it were wrong, in different ways. The
> corrected analysis follows; the empirical finding above — that these decisions surface together —
> reproduces and stands.

### ~~Root cause: there is no supersede/versioning primitive~~ — WITHDRAWN

This was false. `supersedes` and `superseded_by` are both in the schema (`types.go`), the store has
honoured them from the start (`Store.Supersede`, and `Active()` filters `superseded_by`), and 3
decisions already populate them. The claim "verified against raw `/decisions` records" did not hold:
superseded rows are *absent* from that endpoint precisely because the mechanism works.

### Corrected root cause — two distinct defects, neither of them a missing schema field

**(1) A false contradiction from missing subject facets.** `d-cbfde40003b1` and `d-cd39778fbe0c` do
not contradict each other. Checked against the source documents they are cited from, **both are
true, and they are about different tables in different subsystems**:

| Decision | Claim | Source | Verdict |
|---|---|---|---|
| `d-cbfde40003b1` | ClickHouse is the sole store for `orders`, no MySQL dual-write | `docs/ORDERS-ARCHITECTURE.md:27` | **correct**, subject = `orders` |
| `d-cd39778fbe0c` | MySQL `order_events` is the source of truth; CH tables are best-effort replicas | `docs/EVENTS-ARCHITECTURE.md:33` | **correct**, subject = `order_events` |

Both are verbatim-faithful to their sources. They read as a contradiction only because every
decision in the cluster carries the same scope — `["repo:acme"]` — and nothing records *which
table* it governs. Retrieval cannot separate "source of truth for order data" from "source of truth
for metrics data", so it surfaces one when asked about the other.

This report's own method notes state that different-table pairs were "excluded from the strict
count." That rule was correct and was **not applied here** — the pair should never have entered the
9.4%. The strict rate is therefore overstated.

**(2) A real contradiction, in the third decision.** `d-05d7cb7f3b50` ("dual-write
cost/allocation/metrics to both MySQL and ClickHouse") comes from
`docs/warehouse-operations-guide.md:32` and over-generalizes across subsystems. It is true for
metrics, allocation and activity data, and **false for cost** —
`docs/ORDERS-ARCHITECTURE.md:179` addresses this claim directly: *"The prior audit claimed billing
overlay only wrote to MySQL. This is wrong. CH is the sole store. There is no MySQL write path for
cost records."* This decision genuinely conflicts with `d-cbfde40003b1` and is the one item in the
cluster that warrants retirement.

### Corrected recommendation

The audit's remediation — "populate `supersedes` on the stale order-storage decisions" — would have
**destroyed a true fact**, since it treated the metrics decision as a stale version of the cost one
and would have retired it by recency. Do this instead:

1. **Add subject facets**, not supersede edges, to the falsely-conflicting pair: `table:orders`
   on `d-cbfde40003b1`, `table:order_events` + `topic:metrics` on `d-cd39778fbe0c`. Both
   decisions stay live because both are true.
2. **Supersede only `d-05d7cb7f3b50`**, replacing it with a subject-scoped decision that states the
   reconciled position (dual-write for metrics/allocation/activity; ClickHouse-only for
   `orders`).
3. **The primitive needed for step 2 now exists**: the real gap was never a schema field but a
   missing *verb*. `Store.Supersede` was only reachable by capturing a brand-new decision carrying
   `supersedes`, which cannot reconcile two decisions that are *already* recorded — exactly this
   case. `POST /supersede {old,new}` closes that gap and refuses to link into an already-retired
   decision, so no advice can be silently hidden.

### Implication for the headline number

The 9.4% (3/32) strict rate counts at least one pair that this report's own subject rule excludes.
The figure should be treated as an upper bound.

---

## Remediation APPLIED — 2026-07-30, and what it did and did not fix

All three decisions were replaced via capture-with-`supersedes` (content-addressed ids mean adding a
facet mints a new decision, so supersede is the non-destructive edit path). Old rows are retired and
kept in full.

| Retired | Replacement | Change |
|---|---|---|
| `d-cbfde40003b1` | `d-6bb930f8c2af` | + `table:orders`, `topic:cost` |
| `d-cd39778fbe0c` | `d-4b7a026550b5` | + `table:order_events`, `topic:metrics` |
| `d-05d7cb7f3b50` | `d-4a0d6ddb3f3d` | **text corrected** — dual-write claim scoped to metrics/allocation/activity, with `orders` named as the explicit exception |

Corpus: 1643 surfaceable, `sha256[:16] = a8dca1a6d7b7a83a` (was `b65fa0ded6175664`).

### ✅ The genuine contradiction is closed

Across 8 order-storage queries, **0 retired decisions surface**. The pair that previously returned
directly opposite advice now returns consistent advice: `does the aggregator dual write metrics to
clickhouse` surfaces the corrected `d-4a0d6ddb3f3d` (dual-write for metrics/allocation/activity,
*orders excepted*) alongside `d-6bb930f8c2af` (ClickHouse sole store for `orders`) —
two statements that now agree, where before they contradicted.

### ❌ The false contradiction is NOT fixed, and the facets did not fix it

`which database is the source of truth for order data` still returns the **metrics** decision (0.723)
*above* the **cost** decision (0.706). Adding `table:` facets bought nothing for retrieval, for two
compounding reasons:

1. **`table:` is not a hard facet.** `hardFilterPrefixes` in `scope.go` is `["author:", "kind:"]`;
   everything else is a soft topical prior. Verified directly: `scope=table:orders` and
   `scope=table:order_events` return *identical* result sets. The facet is inert as a
   predicate.
2. **Even as a hard facet it would not help here**, because a free-text question carries no facet to
   filter on. The caller would have to already know the subject to disambiguate the answer.

**So the corrected diagnosis has a corrected diagnosis.** Missing subject facets are a *necessary*
condition for the conflation, not a sufficient remedy. The residual failure is a **retrieval**
limitation: both decisions contain their table name in the text, and the bi-encoder still ranks
"source of truth" phrasing above the entity token that distinguishes them. This is the same root
cause as the negation blindness in [`negation.md`](negation.md) — a bi-encoder scoring topical
similarity cannot enforce fine-grained distinctions, whether of polarity or of entity — and it wants
the same fix: a **cross-encoder re-ranker** over the top-k (*Nogueira & Cho, "Passage Re-ranking with
BERT," arXiv:1901.04085*), which jointly reads query and document instead of comparing independently
encoded vectors. That bi-encoders are worst and cross-encoders best on exactly this class of
distinction is measured in *Weller et al., "NevIR," EACL 2024*. See RESEARCH.md §10d, which now
records that this single build closes negation blindness **and** entity conflation together.

**Concrete next steps**, in increasing order of cost:

- Add `table:` (and `service:`) to `hardFilterPrefixes` — one line, makes explicitly-faceted queries
  correct. Does not help un-faceted queries.
- Extract the subject entity from the query and match it against decision facets before ranking.
- Cross-encoder re-rank of the top-k, which addresses entity *and* polarity together.

**Honest status: 1 of 2 defects in this cluster is fixed.** The dangerous one — genuinely
conflicting advice — is closed. The confusing one — a true statement about metrics answering a
question about cost — survives, and is now correctly attributed to retrieval rather than to metadata.

---

### Method notes
- Judged a *pair* contradictory only when both decisions assert opposite claims about the **same
  subject**; different-but-compatible facets (different layers/endpoints/tables) were excluded from
  the strict count and listed under "Borderline."
- Raw records inspected to confirm no `superseded_by`/version field exists and all involved
  decisions are `quarantined:false`, created 2026-07-29 within minutes of each other (co-live).
