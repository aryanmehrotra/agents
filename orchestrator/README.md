# orchestrator

The multi-agent **front door**. It receives a user query, uses an LLM to route it to the right
specialist (`data` / `support` / `kb` / `review` / `redact`), and calls that agent over a **resilient GoFr HTTP
service** — circuit breaker + retry + rate limiter + health check — all from config, no handler
plumbing.

Because the orchestrator and the specialist both export traces, one `/assistant` call becomes a single
**distributed trace across two services**.

## Endpoint

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| POST | `/assistant` | `X-Api-Key` | classify the query, route to a specialist, return its answer |

Response: `{ "route": "...", "routed_to": "...-agent", "response": <specialist payload> }`

## GoFr features it demonstrates

```go
app.AddHTTPService("data-agent", "http://localhost:8000",
    &service.CircuitBreakerConfig{Threshold: 4, Interval: 2 * time.Second}, // trip on repeated failure
    &service.RateLimiterConfig{Requests: 20, Window: time.Second, Burst: 25}, // cap outbound load
    &service.HealthConfig{HealthEndpoint: ".well-known/health-check"},        // probe when open
)
app.EnableAPIKeyAuth("agents-demo-key")   // front-door auth
```

- **Circuit breaker + retry** on every agent-to-agent call
- **Rate limiting** on the outbound calls
- **Health checks** used by the breaker
- **API-key auth** on the front door
- **Distributed tracing** propagated across services automatically

## Run

Start the shim + all three specialists first, then:

```bash
cp configs/.env.local configs/.env && go run .    # :8080

# or with Groq
cp configs/.env.example configs/.env   # add GROQ_API_KEY
go run .
```

## Try it

```bash
# no key → 401
curl -i -s localhost:8080/assistant -d '{"query":"hi"}' | head -1

# routed to data-agent
curl -s localhost:8080/assistant -H 'X-Api-Key: agents-demo-key' \
  -d '{"query":"what is our total shipped revenue?"}'

# routed to support-agent
curl -s localhost:8080/assistant -H 'X-Api-Key: agents-demo-key' \
  -d '{"query":"the app crashes on login with a nil pointer panic"}'

# routed to kb-agent
curl -s localhost:8080/assistant -H 'X-Api-Key: agents-demo-key' \
  -d '{"query":"how many annual leave days do I get?"}'

# routed to code-review-agent
curl -s localhost:8080/assistant -H 'X-Api-Key: agents-demo-key' \
  -d '{"query":"--- a/user.go\n+++ b/user.go\n@@ -1,3 +1,3 @@\n-old\n+new"}'

# routed to pii-redaction-agent
curl -s localhost:8080/assistant -H 'X-Api-Key: agents-demo-key' \
  -d '{"query":"please redact this text: contact John Doe at john.doe@example.com"}'
```

Point the specialists elsewhere with `DATA_AGENT_URL` / `SUPPORT_AGENT_URL` / `KB_AGENT_URL` /
`CODE_REVIEW_AGENT_URL` / `PII_REDACTION_AGENT_URL`.
