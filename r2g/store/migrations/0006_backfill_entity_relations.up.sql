-- Backfill canonical entities + bi-temporal relations from the
-- existing per-document extraction tables (documents_entities,
-- documents_relationships). Lossy by design: relations whose subject
-- or object did not resolve in the original per-document extraction
-- are skipped (subject_id/object_id NULL in source).
--
-- Kept separate from 0005 so a backfill issue doesn't dirty the
-- schema migration. canonical_name uses lower(name) to match the
-- Go-side normalizeName function in r2g/store/extract.go.

-- Step 1: canonical entities, one per distinct normalized name across
-- all per-document extractions. ON CONFLICT swallows duplicates
-- (entities_canonical_active enforces uniqueness on canonical_name
-- where merged_into_id IS NULL).
INSERT INTO deepresearch.entities (name, canonical_name, type, first_seen, last_seen, mention_count)
SELECT
  MIN(de.name)                       AS name,
  lower(de.name)                     AS canonical_name,
  MIN(de.category)                   AS type,
  MIN(de.created_at)                 AS first_seen,
  MAX(de.updated_at)                 AS last_seen,
  COUNT(*)                           AS mention_count
FROM deepresearch.documents_entities de
WHERE de.name IS NOT NULL AND de.name <> ''
GROUP BY lower(de.name)
ON CONFLICT (canonical_name) WHERE merged_into_id IS NULL DO NOTHING;

-- Step 2: bi-temporal relations. Resolve subject/object names through
-- documents_entities (per-doc) into canonical entity IDs via
-- canonical_name. Drop rows that can't resolve on either side.
INSERT INTO deepresearch.entity_relations (
  subject_id, predicate, object_id,
  valid_from, valid_to, recorded_at, invalidated_at,
  evidence_doc_ids, confidence, provenance
)
SELECT
  s.id                               AS subject_id,
  dr.predicate                       AS predicate,
  o.id                               AS object_id,
  NULL::timestamptz                  AS valid_from,
  NULL::timestamptz                  AS valid_to,
  dr.created_at                      AS recorded_at,
  NULL::timestamptz                  AS invalidated_at,
  ARRAY[dr.parent_id]::uuid[]        AS evidence_doc_ids,
  dr.weight                          AS confidence,
  'backfill'                         AS provenance
FROM deepresearch.documents_relationships dr
JOIN deepresearch.entities s
  ON s.canonical_name = lower(dr.subject) AND s.merged_into_id IS NULL
JOIN deepresearch.entities o
  ON o.canonical_name = lower(dr.object)  AND o.merged_into_id IS NULL
WHERE dr.subject IS NOT NULL AND dr.subject <> ''
  AND dr.object  IS NOT NULL AND dr.object  <> ''
  AND dr.predicate IS NOT NULL AND dr.predicate <> '';
