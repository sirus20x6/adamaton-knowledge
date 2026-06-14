-- Add H-Mem rollup-tree columns to deepresearch.documents.
--
-- kind                 polymorphic discriminator: chunk | skill | claim | agent-memory | run-summary | ...
--                      (existing R2R-compat document rows get the 'chunk' default.)
-- parent_id            self-reference for rollup tree; NULL for level-0 leaves.
-- level                0 for leaves, 1+ for cluster/topic/run summaries.
-- source_episode_ids   provenance back to deepresearch.episodes (created in 0002).
--
-- The pre-existing 'type' column (R2R's document_type, e.g. 'pdf'|'txt') is
-- orthogonal to 'kind' and left untouched.

ALTER TABLE deepresearch.documents
  ADD COLUMN kind text NOT NULL DEFAULT 'chunk',
  ADD COLUMN parent_id uuid REFERENCES deepresearch.documents(id) ON DELETE SET NULL,
  ADD COLUMN level int NOT NULL DEFAULT 0,
  ADD COLUMN source_episode_ids uuid[];

CREATE INDEX documents_kind_level ON deepresearch.documents (kind, level);
CREATE INDEX documents_parent_partial ON deepresearch.documents (parent_id, level)
  WHERE parent_id IS NOT NULL;
