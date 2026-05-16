# skills-rae deployment runbook

## Pi-side bring-up (the path actually used 2026-05-15)

The Pi runs docker-compose; skills-rae + skills-rae-worker are containerized.
Both binaries are pure-Go (no cgo) so we cross-compile on the workstation and
build a slim runtime image on the Pi.

### 1. Cross-compile on workstation

```sh
cd /thearray/git/evo
make pi-skills-rae pi-skills-rae-worker
# Outputs: bin/skills-rae.arm64 (15 MB) + bin/skills-rae-worker.arm64 (24 MB)
```

### 2. Ship binaries + build images on Pi

```sh
scp bin/skills-rae.arm64 bin/skills-rae-worker.arm64 pi5:~/skills-rae-build/
scp skills-rae/deploy/Dockerfile.runtime pi5:~/skills-rae-build/
scp skills-rae/deploy/Dockerfile.worker  pi5:~/skills-rae-build/
ssh pi5 'cd ~/skills-rae-build \
    && docker build -f Dockerfile.runtime -t evo-skills-rae:dev . \
    && docker build -f Dockerfile.worker  -t evo-skills-rae-worker:dev .'
```

### 3. Apply migration

Migration `0006_skills_rae` lives in `evolve/store/migrations/` and gets
embedded into the evo binary. Until `evo-worker` (or some other consumer of
`evolve/store.Open`) is rebuilt and restarted, apply it manually:

```sh
scp evolve/store/migrations/0006_skills_rae.up.sql pi5:/tmp/
ssh pi5 'cd ~/deepresearch && docker compose exec -T postgres \
    psql -U postgres -d postgres < /tmp/0006_skills_rae.up.sql'
ssh pi5 'cd ~/deepresearch && docker compose exec -T postgres \
    psql -U postgres -d postgres -c "UPDATE schema_migrations_evo SET version = 6, dirty = false;"'
```

### 4. Add compose entries

See `skills-rae/deploy/compose-fragment.yml` — paste into
`~/deepresearch/docker-compose.yml` before the top-level `volumes:` key.

### 5. Add Caddy proxy

See `skills-rae/deploy/caddy-fragment` — paste into
`~/deepresearch/infra/Caddyfile` *before* the `@plugin_host` matcher (and
therefore before the `@api` fallback) so `/platform/skills/rae/*` lands on
skills-rae and not the FastAPI backend.

After editing the file *in place* via `mv` you must `docker compose restart
frontend` — bind-mounted files survive `caddy reload` only when the inode
doesn't change. `mv newfile oldfile` replaces the inode and the container
keeps reading the old one until restart.

### 6. Start services + bootstrap index

```sh
ssh pi5 'cd ~/deepresearch && docker compose up -d skills-rae skills-rae-worker'

# Trigger the one-shot full corpus reindex.
ssh pi5 'docker exec deepresearch-temporal-1 temporal --address temporal:7233 \
    workflow start --task-queue skills-rae \
    --type ReindexAllSkillsWorkflow \
    --workflow-id skills-rae-bootstrap-$(date +%s) \
    --input "{\"concurrency\":4,\"continue_as_new_every\":200}"'
```

On a 1088-skill corpus this takes 10-15 minutes wall-clock dominated by
bge-m3 embedding calls (sidecar lives on the workstation; the network hop
+ HF tokenizer churn is the slow part). Progress query:

```sh
ssh pi5 'cd ~/deepresearch && docker compose exec -T postgres psql -U postgres -d postgres \
    -c "SELECT COUNT(*) AS skills, COUNT(rae_indexed_at) AS indexed FROM evo.skills; \
        SELECT COUNT(*) AS subunits FROM evo.skill_subunits; \
        SELECT COUNT(*) AS edges FROM evo.skill_subunit_edges; \
        SELECT COUNT(*) AS communities, AVG(member_count)::int AS avg_size FROM evo.skill_communities;"'
```

### 7. Flip the delegator

Edit `~/.claude.json` and set
`mcpServers.delegator.env.SKILLS_RAE_URL=https://deepresearch.local/platform/skills/rae`.
The delegator's `skillsclient.HTTPClient` reads this on construction; presence
of the URL is the feature flag.

Rebuild the delegator-mcp binary so it picks up the new SkillRAE code:

```sh
cd /thearray/git/evo
go build -o bin/delegator-mcp ./mcp/cmd/delegator-mcp
```

Then either restart your Claude Code session or `pkill delegator-mcp` —
Claude Code auto-respawns it.

### Verification

```sh
# Health.
curl -sk https://deepresearch.local/platform/skills/rae/healthz
# -> {"db":true,"service":"skills-rae","status":"ok"}

# Compile a real query.
curl -sk -X POST https://deepresearch.local/platform/skills/rae/v1/rae/compile \
    -H 'content-type: application/json' \
    -d '{"query":"rename a variable in a python file","top_k":5}'

# Trace ledger.
ssh pi5 'cd ~/deepresearch && docker compose exec -T postgres psql -U postgres -d postgres \
    -c "SELECT variant, COUNT(*), AVG(elapsed_ms)::int AS avg_ms FROM evo.skill_rae_traces GROUP BY 1;"'
```

### Rollback

Set `SKILLS_RAE_URL=` (empty) in `~/.claude.json` — the delegator falls back
to the flat search path automatically. `docker compose stop skills-rae
skills-rae-worker` is optional; the services are inert when nothing calls
them.
