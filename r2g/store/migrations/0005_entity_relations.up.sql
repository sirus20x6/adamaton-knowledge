-- Bi-temporal entity graph edges (Zep/Graphiti shape).
--
-- valid_from / valid_to        when the relation holds in the world
-- recorded_at / invalidated_at when we learned about it / decided it's wrong
-- predicate_norm               generated column for resolution-friendly lookup
--                              ("works_at" / "worksAt" / "work at" all collapse)
-- evidence_doc_ids /            provenance back to the source episodes/docs
--   evidence_episode_ids       (uuid[] until we need a join table)
-- provenance                   'extracted' (LLM), 'backfill' (cut 1 import),
--                              'asserted' (manual), 'derived' (graph reasoning)
--
-- Cut 2 writes here with naive dedup on (subject_id, predicate_norm, object_id)
-- with no invalidation. Cut 5 (Graphiti port) introduces edge resolution and
-- temporal invalidation that updates invalidated_at when contradictory
-- evidence arrives.

CREATE TABLE deepresearch.entity_relations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  subject_id uuid NOT NULL REFERENCES deepresearch.entities(id) ON DELETE CASCADE,
  predicate text NOT NULL,
  predicate_norm text GENERATED ALWAYS AS (lower(btrim(predicate))) STORED,
  object_id uuid NOT NULL REFERENCES deepresearch.entities(id) ON DELETE CASCADE,
  valid_from timestamptz,
  valid_to timestamptz,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  invalidated_at timestamptz,
  evidence_doc_ids uuid[],
  evidence_episode_ids uuid[],
  confidence real,
  provenance text NOT NULL DEFAULT 'extracted'
);

CREATE INDEX er_subject_time
  ON deepresearch.entity_relations (subject_id, valid_from DESC NULLS LAST, valid_to);
CREATE INDEX er_object_time
  ON deepresearch.entity_relations (object_id, valid_from DESC NULLS LAST);
CREATE INDEX er_current_state
  ON deepresearch.entity_relations (subject_id, predicate_norm, object_id)
  WHERE invalidated_at IS NULL;
CREATE INDEX er_provenance
  ON deepresearch.entity_relations (provenance);
