#!/usr/bin/env bash
# Quick A/B: for each query in QUERIES, hit /v1/rae/search and the legacy
# /api/v1/skills/search, then print the top-K skill IDs from each and the
# Jaccard overlap. NOT a benchmark -- this is a "is RAE doing something
# different?" sanity check. Use this before tuning weights so you have a
# baseline of how often RAE diverges from flat retrieval.
#
# Usage:
#   ./ab_compare.sh             # uses the canned QUERIES below
#   QUERY="custom q" ./ab_compare.sh
set -euo pipefail

RAE="${SKILLS_RAE_URL:-https://deepresearch.local/platform/skills/rae}"
FLAT="${FLAT_SKILLS_URL:-https://deepresearch.local/api/v1/skills/search}"
TOP_K="${TOP_K:-5}"
CURL_OPTS=${CURL_OPTS:--sSk}

if [[ -n "${QUERY:-}" ]]; then
    queries=("$QUERY")
else
    queries=(
        "rename a variable in a python file"
        "search google scholar for a paper"
        "extract a method out of a long function"
        "review a github pull request"
        "convert latex to plain text"
        "run a smoke test against a deployed service"
        "fix a flaky integration test"
        "deploy an arm64 docker image to a raspberry pi"
    )
fi

for q in "${queries[@]}"; do
    printf '\n\033[1m== %s ==\033[0m\n' "$q"
    rae_ids=$(curl $CURL_OPTS -X POST "$RAE/v1/rae/search" \
        -H 'content-type: application/json' \
        -d "{\"query\":\"$q\",\"top_k\":$TOP_K}" \
        | python3 -c "
import json, sys
r = json.loads(sys.stdin.read())
for s in r.get('selected_skills', [])[:$TOP_K]:
    print(s['skill_id'], s['name'], 'score=%.3f' % s['score'])
")
    flat_ids=$(curl $CURL_OPTS -X POST "$FLAT" \
        -H 'content-type: application/json' \
        -d "{\"query\":\"$q\",\"limit\":$TOP_K}" \
        | python3 -c "
import json, sys
r = json.loads(sys.stdin.read())
for h in r.get('hits', [])[:$TOP_K]:
    print(h.get('skill_id',''), h.get('skill_name','?'), 'score=%.3f' % h.get('score',0))
" || echo '(flat unavailable)')
    printf '  \033[36mRAE:\033[0m\n'
    echo "$rae_ids" | sed 's/^/    /'
    printf '  \033[35mFLAT:\033[0m\n'
    echo "$flat_ids" | sed 's/^/    /'
    rae_set=$(echo "$rae_ids" | awk '{print $1}' | sort -u)
    flat_set=$(echo "$flat_ids" | awk '{print $1}' | sort -u)
    if [[ -n "$rae_set" && -n "$flat_set" ]]; then
        common=$(comm -12 <(echo "$rae_set") <(echo "$flat_set") | wc -l | tr -d ' ')
        rae_only=$(comm -23 <(echo "$rae_set") <(echo "$flat_set") | wc -l | tr -d ' ')
        flat_only=$(comm -13 <(echo "$rae_set") <(echo "$flat_set") | wc -l | tr -d ' ')
        printf '  \033[33moverlap:\033[0m %d  rae-only: %d  flat-only: %d\n' "$common" "$rae_only" "$flat_only"
    fi
done
