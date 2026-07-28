#!/bin/bash
# Real session against the running memory-agent (GoFr 1.58 + SurrealDB, Ollama embeddings).
AG=http://localhost:8006/chat
C=$'\e[36m'; G=$'\e[32m'; O=$'\e[38;5;214m'; D=$'\e[90m'; W=$'\e[97m'; B=$'\e[1m'; Y=$'\e[33m'; R=$'\e[0m'
SID="demo-$RANDOM"

say(){ curl -s --max-time 150 -X POST "$AG" -H 'Content-Type: application/json' \
       -d "{\"session_id\":\"$1\",\"message\":\"$2\"}"; }

printf "${D}# three facts, stated plainly${R}\n"
for f in "My name is Aryan and I love hiking in the mountains." \
         "I work as a backend engineer and my favorite language is Go." \
         "I'm allergic to peanuts."; do
  say "$SID" "$f" >/dev/null
  printf "  ${G}✓${R} ${D}remembered${R}  %s\n" "$f"
done

ask(){
  local resp ans
  resp=$(say "$SID" "$1")
  printf "\n${C}${B}❯ %s${R}\n" "$1"
  # print the real ranked recall list (top 2) so the relevant fact is always visible
  printf '%s' "$resp" | python3 -c "
import sys,json
for m in (json.load(sys.stdin)['data']['recalled'] or [])[:2]:
    print(m['content']+'\t'+str(round(m['score'],3)))
" | while IFS=$'\t' read -r c s; do
    printf "  ${D}recalled${R}  ${O}%-52s${R} ${D}cosine${R} ${G}%s${R}\n" "$c" "$s"
  done
  ans=$(printf '%s' "$resp" | python3 -c "
import sys,json
a=json.load(sys.stdin)['data']['answer'].strip().split('. ')[0]
print(a if len(a)<=74 else a[:74].rsplit(' ',1)[0]+'…')")
  printf "  ${D}answer  ${R}  ${W}%s${R}\n" "$ans"
}

ask "recommend a fun weekend outdoor activity for me"
ask "suggest a dessert I could safely eat"
ask "what programming topic should I study next"

# brand-new session — proves the model itself is stateless
STR="stranger-$RANDOM"
sans=$(say "$STR" "what is my name?" | python3 -c "
import sys,json
a=json.load(sys.stdin)['data']['answer'].strip().replace(chr(10),' ')
a=a.split(',')[0].split('. ')[0]
print(a if len(a)<=74 else a[:74].rsplit(' ',1)[0])")
printf "\n${D}# brand-new session — the model has no memory of its own${R}\n"
printf "${C}${B}❯ what is my name?${R}  ${D}(new session)${R}\n"
printf "  ${D}answer  ${R}  ${W}%s.${R}\n" "$sans"

printf "\n${D}none of the queries share a keyword with the facts — recall is ${R}${Y}semantic${R}${D},${R}\n"
printf "${D}via GoFr's ${R}${O}Embedder${R}${D} + SurrealDB ${R}${O}vector::similarity::cosine${R}${D}.${R}\n"
