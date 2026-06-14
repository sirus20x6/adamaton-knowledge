DROP INDEX IF EXISTS deepresearch.documents_parent_partial;
DROP INDEX IF EXISTS deepresearch.documents_kind_level;

ALTER TABLE deepresearch.documents
  DROP COLUMN IF EXISTS source_episode_ids,
  DROP COLUMN IF EXISTS level,
  DROP COLUMN IF EXISTS parent_id,
  DROP COLUMN IF EXISTS kind;
