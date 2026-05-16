#!/usr/bin/env bash
# Smoke test for skills-rae against the Pi deployment.
# Run from anywhere; needs SSH access to pi5 and the Caddy proxy at
# https://deepresearch.local/platform/skills/rae/.
set -euo pipefail

RAE="${SKILLS_RAE_URL:-https://deepresearch.local/platform/skills/rae}"
CURL_OPTS="${CURL_OPTS:--sSk}"
HOST="${HOST:-pi5}"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; exit 1; }

bold "1. healthz"
status=$(curl $CURL_OPTS "$RAE/healthz")
echo "    $status"
echo "$status" | grep -q '"status":"ok"' && ok healthy || fail "service unhealthy"

bold "2. corpus state"
ssh "$HOST" 'cd ~/deepresearch && docker compose exec -T postgres psql -U postgres -d postgres -c "
    SELECT COUNT(*) AS skills,
           COUNT(rae_indexed_at) AS indexed,
           COUNT(description_embedding) AS embedded,
           COUNT(community_id) AS in_community
      FROM evo.skills;
    SELECT COUNT(*) AS subunits, COUNT(embedding) AS subunit_embeddings FROM evo.skill_subunits;
    SELECT COUNT(*) AS communities, AVG(member_count)::int AS avg_size FROM evo.skill_communities;
    SELECT COUNT(*) AS edges FROM evo.skill_subunit_edges;
"'

bold "3. compile against a real query"
echo "    query: rename a variable in a python file"
resp=$(curl $CURL_OPTS -X POST "$RAE/v1/rae/compile" \
    -H 'content-type: application/json' \
    -d '{"query":"rename a variable in a python file","top_k":5,"budget_tokens":1500}')
echo "$resp" | python3 -c "
import json, sys
r = json.loads(sys.stdin.read())
print('    selected_skills:', len(r['selected_skills']))
for s in r['selected_skills'][:5]:
    print('      -', s['name'], 'score=%.3f boosted=%s' % (s['score'], s['boosted']))
print('    rescue_attached:', len(r['rescue_attached']))
print('    context_chars:', len(r['context']))
d = r['diagnostics']
print('    top_down_labels:', d['top_down_labels'])
print('    stage_ms:', d['stage_ms'])
print('    bottom_up_hits:', d['bottom_up_hits'])
print('    l1_hits:', d['l1_hits'])
print()
print('    --- context preview ---')
print('\n'.join(r['context'].splitlines()[:30]))
"

bold "4. trace ledger"
ssh "$HOST" 'cd ~/deepresearch && docker compose exec -T postgres psql -U postgres -d postgres -c "
    SELECT variant, COUNT(*), AVG(elapsed_ms)::int AS avg_ms, AVG(rescued_count)::int AS avg_rescue
      FROM evo.skill_rae_traces
     GROUP BY variant;
"'

bold "5. compare against flat search (same query)"
flat=$(curl $CURL_OPTS -X POST https://deepresearch.local/api/v1/skills/search \
    -H 'content-type: application/json' \
    -d '{"query":"rename a variable in a python file","limit":5}' 2>/dev/null || echo '{"hits":[]}')
echo "$flat" | python3 -c "
import json, sys
try:
    r = json.loads(sys.stdin.read())
    hits = r.get('hits', [])
    print('    flat search hits:', len(hits))
    for h in hits[:5]:
        print('      -', h.get('skill_name','?'), 'score=%.3f' % h.get('score',0))
except Exception as e:
    print('    flat search unavailable:', e)
"

bold "DONE"
