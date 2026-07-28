# code-review-agent

Reviews a unified diff with an LLM and returns **structured, file/line-anchored comments** — the way
a human reviewer would leave inline PR comments. AI code review is one of the most heavily adopted
agent use cases right now, alongside AI coding assistants themselves.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/review` | structured review → JSON `{summary, risk, comments:[{file, line, severity, comment}]}` |
| POST | `/review/stream` | same review as streamed prose (SSE) |

## Run

```bash
# keyless (start ../../../localtest/claude-openai-shim first)
cp configs/.env.local configs/.env && go run .

# or with Groq
cp configs/.env.example configs/.env   # add GROQ_API_KEY
go run .
```

## Try it

```bash
curl -s localhost:8003/review -d '{
  "title": "Add user lookup by email",
  "diff": "--- a/user.go\n+++ b/user.go\n@@ -10,6 +10,10 @@\n func GetUser(id string) (*User, error) {\n \treturn db.Find(id)\n }\n+\n+func GetUserByEmail(email string) (*User, error) {\n+\tquery := \"SELECT * FROM users WHERE email = '\" + email + \"'\"\n+\treturn db.Raw(query)\n+}\n"
}'
# → {"summary":"…","risk":"high","comments":[{"file":"user.go","line":15,"severity":"blocker","comment":"string-concatenated SQL is injectable — use a parameterized query"}]}

# streamed prose review (note -N for no buffering)
curl -sN localhost:8003/review/stream -d '{"title":"...","diff":"..."}'
```

The review prompt lives in `main.go` (`reviewSystem`) — tune severity labels or add house style
rules to fit your own team's review conventions.
