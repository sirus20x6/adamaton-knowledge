#!/usr/bin/env python3
"""Mine https://skills.md/api/skills and load every entry into evo.skills.

The skills.md marketplace lists CLI-callable skills (run via the
``skills`` binary from ``bun install -g @hasna/skills``). They don't
ship a SKILL.md body the way our existing skills do — just metadata +
a one-line cliCommand. So we synthesize a SKILL.md-shaped body from
the metadata. That gives the SkillRAE subunit extractor real
procedural / element / constraint lines to chew on (the ``skills run
<name>`` line is procedural; the env-var list yields element refs;
the pricing/tier line yields constraints).

Idempotent: skills are namespaced under the ``skillsmd-`` prefix and
deduplicated via the ``source_sha`` UNIQUE index on evo.skills. Re-
running on an unchanged catalog is a no-op.

Run pattern (workstation → Pi):

    python3 skills-rae/deploy/import_skills_md.py | \\
        ssh pi5 'cd ~/deepresearch && docker compose exec -T postgres \\
            psql -U postgres -d postgres -v ON_ERROR_STOP=1'

After loading, trigger the SkillRAE indexer to embed + cluster the
new rows:

    ssh pi5 'docker exec deepresearch-temporal-1 temporal \\
        --address temporal:7233 workflow start \\
        --task-queue skills-rae \\
        --type ReindexAllSkillsWorkflow \\
        --workflow-id skills-rae-skillsmd-$(date +%s) \\
        --input "{\\"concurrency\\":4,\\"continue_as_new_every\\":200}"'

The script prints a final RAISE NOTICE summary so the operator sees
how many rows landed.
"""
from __future__ import annotations

import hashlib
import json
import sys
import urllib.request

CATALOG_URL = "https://skills.md/api/skills"
NAME_PREFIX = "skillsmd-"
ORIGIN = "skills.md"


def fetch_catalog() -> list[dict]:
    with urllib.request.urlopen(CATALOG_URL, timeout=30) as r:
        return json.load(r)


def synth_body(skill: dict) -> str:
    """Generate a SKILL.md-shaped body from the marketplace metadata.

    The structure mirrors what our deterministic subunit extractor
    looks for: numbered procedural steps, element-style path/dir
    references (env vars + system deps), and constraint phrases
    ("This skill should be used when…", pricing notes).
    """
    name = skill["name"]
    display = skill.get("displayName") or name
    desc = skill.get("description", "")
    category = skill.get("category", "")
    cli = skill.get("cliCommand", f"skills run {name}")
    env_vars = skill.get("envVars") or []
    sys_deps = skill.get("systemDeps") or []
    pricing = skill.get("pricing") or {}
    tier = pricing.get("tier", "free")
    cost = pricing.get("formattedCost", "free")
    unit = pricing.get("billingUnit", "run")

    lines: list[str] = []
    lines.append(f"# {display}")
    lines.append("")
    lines.append(desc)
    lines.append("")
    lines.append("## When to use")
    lines.append("")
    lines.append(
        f"This skill should be used when you need {desc.lower().rstrip('.')}. "
        f"Published in the skills.md marketplace under the "
        f"\"{category}\" category."
    )
    lines.append("")
    lines.append("## Procedure")
    lines.append("")
    lines.append(
        "1. Install the skills CLI globally with `bun install -g @hasna/skills`."
    )
    if env_vars:
        lines.append(
            f"2. Configure environment variables: {', '.join('`' + v + '`' for v in env_vars)}."
        )
        next_step = 3
    else:
        next_step = 2
    lines.append(f"{next_step}. Run the skill with `{cli}`.")
    lines.append(
        f"{next_step + 1}. Inspect the output and pipe it into the downstream step."
    )
    lines.append("")
    lines.append("## Environment")
    lines.append("")
    if env_vars:
        lines.append("- Required environment variables:")
        for v in env_vars:
            lines.append(f"  - `{v}`")
    else:
        lines.append("- No environment variables required.")
    if sys_deps:
        lines.append("- System dependencies:")
        for d in sys_deps:
            lines.append(f"  - `{d}`")
    else:
        lines.append("- No system dependencies required.")
    lines.append("")
    lines.append("## Constraints")
    lines.append("")
    lines.append(
        f"- This skill requires the @hasna/skills CLI to be installed before use."
    )
    if tier == "premium":
        lines.append(
            f"- This skill is premium ({cost} per {unit}); never run it in "
            f"automated loops without budget controls."
        )
    else:
        lines.append(
            "- This skill is free to run but each call still consumes "
            "@hasna/skills service quota — avoid wasteful retries."
        )
    if env_vars:
        lines.append(
            f"- Must reject runs when any of the required environment "
            f"variables ({', '.join(env_vars)}) is unset."
        )
    lines.append("")
    lines.append("## References")
    lines.append("")
    lines.append(f"- Marketplace listing: https://skills.md/skills/{name}")
    lines.append(f"- CLI invocation: `{cli}`")
    lines.append(f"- Provider: skills.md ({skill.get('source', 'unknown')})")
    return "\n".join(lines).rstrip() + "\n"


def sql_escape(s: str) -> str:
    return s.replace("'", "''")


def sql_array(values: list[str]) -> str:
    if not values:
        return "ARRAY[]::TEXT[]"
    return "ARRAY[" + ", ".join("'" + sql_escape(v) + "'" for v in values) + "]"


def emit_sql(skills: list[dict]) -> str:
    out: list[str] = []
    out.append("BEGIN;")
    out.append(
        "-- skills.md marketplace import. Re-runnable: ON CONFLICT (name) "
        "DO NOTHING. The source_sha column also has a UNIQUE index but it's "
        "partial (WHERE source_sha IS NOT NULL); ON CONFLICT can't target a "
        "partial index without a WHERE clause, so we key on name instead. "
        "Names are namespaced under skillsmd- so collisions only happen on "
        "true re-runs of the same upstream catalog."
    )
    for s in skills:
        name = NAME_PREFIX + s["name"]
        body = synth_body(s)
        body_sha = hashlib.sha256(body.encode()).hexdigest()
        tags = list(s.get("tags") or [])
        # Tag the imported corpus so the SPA can filter on it.
        tags.append("skillsmd-imported")
        tier = (s.get("pricing") or {}).get("tier")
        if tier:
            tags.append(f"tier-{tier}")
        # Community = category (matches the "find-bugs", "os-scripting"
        # naming convention the existing corpus uses).
        community = (s.get("category") or "").lower().replace(" & ", "-").replace(" ", "-")
        source_url = f"https://skills.md/skills/{s['name']}"
        # Description: keep it under the column's TEXT type — no
        # length cap practically. Trim trailing whitespace.
        desc = (s.get("description") or "").strip()
        when_to_use = (
            f"Use this skill when you need {desc.lower().rstrip('.')}."
        )
        cli_for_example = s.get("cliCommand") or f"skills run {s['name']}"
        example = f"$ {cli_for_example}"

        out.append("INSERT INTO evo.skills (")
        out.append("    name, description, body, when_to_use, example,")
        out.append("    community, tags, depends_on, origin, source_url, source_sha")
        out.append(") VALUES (")
        out.append(f"    '{sql_escape(name)}',")
        out.append(f"    '{sql_escape(desc)}',")
        # Body is multi-line; use dollar-quoting to skip escape hell.
        out.append("    $skill$" + body + "$skill$,")
        out.append(f"    '{sql_escape(when_to_use)}',")
        out.append(f"    '{sql_escape(example)}',")
        out.append(f"    '{sql_escape(community)}',")
        out.append(f"    {sql_array(tags)},")
        out.append("    ARRAY[]::UUID[],")
        out.append(f"    '{ORIGIN}',")
        out.append(f"    '{sql_escape(source_url)}',")
        out.append(f"    '{body_sha}'")
        out.append(") ON CONFLICT (name) DO NOTHING;")
    out.append(
        "DO $$ DECLARE n INT; BEGIN "
        "SELECT COUNT(*) INTO n FROM evo.skills WHERE origin = 'skills.md'; "
        "RAISE NOTICE 'skills.md skills present in evo.skills: %', n; "
        "END $$;"
    )
    out.append("COMMIT;")
    return "\n".join(out) + "\n"


def main() -> int:
    skills = fetch_catalog()
    print(f"-- Fetched {len(skills)} skills from {CATALOG_URL}", file=sys.stderr)
    print(emit_sql(skills))
    return 0


if __name__ == "__main__":
    sys.exit(main())
