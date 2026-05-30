-- Run manually on the platform Postgres. Owner of platform.document_figures controls schema migrations.
-- Supports DeleteStaleFigures (reindex/activities/queue.go) and r2g's
-- DeleteDocument cascade (r2g/store/search.go), both of which delete by
-- document_id. Without this index the per-paper old-doc figure cleanup
-- does a sequential scan of platform.document_figures on every reindex.
CREATE INDEX CONCURRENTLY IF NOT EXISTS document_figures_document_id_idx
  ON platform.document_figures (document_id);
