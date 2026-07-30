#!/usr/bin/env bash
# org-memory recall hook for Claude Code (UserPromptSubmit).
# Before Claude thinks, inject the prior decisions relevant to your prompt, scoped to this repo.
#
# Injection is confidence-shaped (v2 eval finding: a weak agent under-applies a flat "advisory list"
# even when the right decision is present — surfacing != compliance). So when the top match is CONFIDENT
# (not weak) we lead with it as an authoritative DIRECTIVE and demote the rest to "also consider"; when
# the whole set is weak we present it as low-confidence advisory. Wire it: see integrations/README.md.
set -euo pipefail
BASE="${ORG_MEMORY_URL:-http://localhost:8000}"
input="$(cat)"
prompt="$(printf '%s' "$input" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("prompt",""))' 2>/dev/null || true)"
[ -z "$prompt" ] && exit 0
repo="$(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)")"
qp="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$prompt" 2>/dev/null || true)"
sc="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "repo:${repo}" 2>/dev/null || true)"
curl -s --max-time 3 "${BASE}/recall?q=${qp}&scope=${sc}" 2>/dev/null | python3 -c '
import sys, json
try: d = json.load(sys.stdin)["data"]
except Exception: sys.exit(0)
items = d.get("items", [])
if not items: sys.exit(0)
weak = bool(d.get("weak"))
top = items[0]["decision"]
rest = items[1:]
if not weak:
    print("## Team decision that applies here (org-memory) — follow it unless you have a specific reason not to")
    print("**" + top["what"] + "**")
    if top.get("why"): print("Why: " + top["why"])
    print("Apply this decision’s specific mechanism exactly; do not substitute a generic default.")
    if rest:
        print("\nAlso consider:")
        for it in rest:
            dd = it["decision"]; line = "- " + dd["what"]
            if dd.get("why"): line += " — " + dd["why"]
            print(line)
else:
    print("## Possibly-relevant prior decisions (org-memory) — LOW confidence, verify before relying on these")
    for it in items:
        dd = it["decision"]; line = "- " + dd["what"]
        if dd.get("why"): line += " — " + dd["why"]
        print(line)
' 2>/dev/null || true
