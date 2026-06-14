-- Two tables, not one polymorphic table. Real FKs catch dangling
-- mentions automatically on delete; CASCADE keeps cleanup honest.
-- A UNION ALL view (entity_mentions) preserves the one-query ergonomic
-- for read paths.

CREATE TABLE deepresearch.episode_mentions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  entity_id uuid NOT NULL REFERENCES deepresearch.entities(id) ON DELETE CASCADE,
  episode_id uuid NOT NULL REFERENCES deepresearch.episodes(id) ON DELETE CASCADE,
  span_start int,
  span_end int,
  confidence real,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deepresearch.document_mentions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  entity_id uuid NOT NULL REFERENCES deepresearch.entities(id) ON DELETE CASCADE,
  document_id uuid NOT NULL REFERENCES deepresearch.documents(id) ON DELETE CASCADE,
  span_start int,
  span_end int,
  confidence real,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX episode_mentions_entity ON deepresearch.episode_mentions (entity_id);
CREATE INDEX episode_mentions_episode ON deepresearch.episode_mentions (episode_id);
CREATE INDEX document_mentions_entity ON deepresearch.document_mentions (entity_id);
CREATE INDEX document_mentions_document ON deepresearch.document_mentions (document_id);

CREATE VIEW deepresearch.entity_mentions AS
  SELECT id, entity_id, 'episode'::text AS target_kind, episode_id AS target_id,
         span_start, span_end, confidence, created_at
    FROM deepresearch.episode_mentions
  UNION ALL
  SELECT id, entity_id, 'document'::text AS target_kind, document_id AS target_id,
         span_start, span_end, confidence, created_at
    FROM deepresearch.document_mentions;
