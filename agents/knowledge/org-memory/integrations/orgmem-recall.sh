#!/usr/bin/env bash
# org-memory recall hook for Claude Code (UserPromptSubmit).
# Before Claude thinks, inject the 2-3 prior decisions relevant to your prompt, scoped to this repo.
# Wire it: add to ~/.claude/settings.json (see integrations/README.md). Fails silent if the service is down.
set -euo pipefail
BASE="${ORG_MEMORY_URL:-http://localhost:8000}"
input="$(cat)"
prompt="$(printf '%s' "$input" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("prompt",""))' 2>/dev/null || true)"
[ -z "$prompt" ] && exit 0
repo="$(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)")"
ctx="repo:${repo}, ${prompt}"
q="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$ctx" 2>/dev/null || true)"
curl -s --max-time 3 "${BASE}/recall?context=${q}" 2>/dev/null | python3 -c '
import sys,json
try: items=json.load(sys.stdin)["data"].get("items",[])
except Exception: sys.exit(0)
if not items: sys.exit(0)
print("## Relevant prior decisions (org-memory) — consider before acting")
for it in items:
    d=it["decision"]; line="- "+d["what"]
    if d.get("why"): line+=" — "+d["why"]
    print(line)
' 2>/dev/null || true
