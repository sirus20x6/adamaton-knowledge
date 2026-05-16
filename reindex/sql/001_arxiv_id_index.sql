-- Run manually on the platform Postgres. Owner of deepresearch.documents controls schema migrations.
-- This index dramatically speeds up reindex/activities/queue.go QueryWorkSet on large corpora.
CREATE INDEX CONCURRENTLY IF NOT EXISTS documents_arxiv_id_created_idx
  ON deepresearch.documents ((metadata->>'arxiv_id'), created_at DESC)
  WHERE metadata ? 'arxiv_id';
