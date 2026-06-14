-- Canonical entity store, deduped across episodes and documents.
--
-- merged_into_id supports Graphiti-style entity resolution: when an
-- alias gets merged into a canonical entity, we preserve history by
-- keeping the alias row and pointing merged_into_id at the survivor
-- rather than deleting it. Lookups against canonical state filter
-- WHERE merged_into_id IS NULL.
--
-- canonical_name is the resolution key (lowercased / normalized name);
-- name keeps the original surface form for display.
--
-- Cut 2 writes rows here with naive dedup (insert-or-no-op by
-- canonical_name). Cut 5 (Graphiti port) introduces the resolution
-- algorithm that populates merged_into_id and updates mention_count.

CREATE TABLE deepresearch.entities (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  canonical_name text NOT NULL,
  type text,
  embedding vector(1024),
  merged_into_id uuid REFERENCES deepresearch.entities(id) ON DELETE SET NULL,
  first_seen timestamptz NOT NULL DEFAULT now(),
  last_seen timestamptz NOT NULL DEFAULT now(),
  mention_count int NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX entities_canonical_active ON deepresearch.entities (canonical_name)
  WHERE merged_into_id IS NULL;
CREATE INDEX entities_merged_into ON deepresearch.entities (merged_into_id)
  WHERE merged_into_id IS NOT NULL;
