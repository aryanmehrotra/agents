#!/usr/bin/env bash
# Boot wrapper for org-memory, used by the LaunchAgent.
#
# Three things a bare `./org-memory` gets wrong at boot, each of which cost time during development:
#
#   1. IT NEEDS AN EMBEDDER. org-memory fails loudly without one (deliberately — a silent fallback
#      would write vectors from a different space into the same store and poison every future
#      cosine). At login, Ollama may not be up yet, so wait for it rather than crash-looping.
#   2. THE PORTS MUST BE FREE. A previous instance still holding the metrics port makes the process
#      exit FATAL at startup with a message that looks nothing like "port in use".
#   3. IT MUST RUN FROM ITS OWN DIRECTORY. The config, the SQLite file and the static assets are all
#      resolved relative to the working directory.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"

# Start Ollama if it isn't already serving; it is a separate user process, not our dependency to own.
if ! curl -sf --max-time 2 "$OLLAMA_URL/api/tags" >/dev/null 2>&1; then
  command -v ollama >/dev/null 2>&1 && { nohup ollama serve >/dev/null 2>&1 & }
fi

# Wait for the embedder rather than crash-loop against it. Bounded: if it never arrives, exit and let
# launchd retry on its own schedule instead of spinning.
for _ in $(seq 1 60); do
  curl -sf --max-time 2 "$OLLAMA_URL/api/tags" >/dev/null 2>&1 && break
  sleep 2
done

if ! curl -sf --max-time 2 "$OLLAMA_URL/api/tags" >/dev/null 2>&1; then
  echo "org-memory: embedder unreachable at $OLLAMA_URL after 120s — not starting" >&2
  exit 1
fi

# Reclaim our own ports from a stale instance. Without this the process dies FATAL on the metrics
# port with an error that does not mention the previous process at all.
pkill -f "$PWD/org-memory" 2>/dev/null
sleep 2

exec ./org-memory
