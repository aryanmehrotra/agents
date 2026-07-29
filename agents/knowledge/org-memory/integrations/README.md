# Use org-memory in your Claude CLI

## 1. Run the service (local, no laptop load)
```bash
cd agents/knowledge/org-memory
ORGMEM_EMBED=local ORG_MEMORY_DB=./org-memory.db ./org-memory   # or EMBED via Ollama for semantic recall
```

## 2. Auto-recall on every prompt (the invisible integration)
Add this to `~/.claude/settings.json` (adjust the path):
```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command",
        "command": "/ABSOLUTE/PATH/agents/knowledge/org-memory/integrations/orgmem-recall.sh" } ] }
    ]
  }
}
```
Now, before Claude answers, the hook injects the 2-3 prior decisions relevant to your prompt (scoped to
the current repo). It's advisory context — Claude reads it; nothing is routed *through* org-memory.

## 3. Capture decisions
- The **console** (http://localhost:8000/static/console.html) → Teach, or
- seed from a repo's docs (see the doc-extraction approach), or
- later: a capture hook.

## What routes through it
Only recall (a read, per prompt) and capture (a write, when you teach). Your Claude chat still goes
straight to Anthropic — org-memory just enriches the prompt going in.
