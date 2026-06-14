-- Raw, append-only episodic capture. Cheap writes; the consolidator
-- worker (cut 2) promotes batches into deepresearch.documents and
-- stamps consolidated_into.
--
-- foresight_until is the EverMemOS "forward-looking memory" signal:
-- callers can mark an episode as anticipating relevance through a
-- future time window (e.g. a calendar event, a deadline). Retrieval
-- with foresight=active filters to rows where NOW() <= foresight_until.
--
-- tombstoned_at lets us retain provenance once content is rolled up
-- without keeping the raw text forever.

CREATE TABLE deepresearch.episodes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL,
  content text NOT NULL,
  embedding vector(1024),
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  occurred_at timestamptz NOT NULL DEFAULT now(),
  foresight_until timestamptz,
  consolidated_into uuid REFERENCES deepresearch.documents(id) ON DELETE SET NULL,
  tombstoned_at timestamptz
);

CREATE INDEX episodes_kind_occurred ON deepresearch.episodes (kind, occurred_at DESC);
CREATE INDEX episodes_foresight_active ON deepresearch.episodes (foresight_until)
  WHERE foresight_until IS NOT NULL AND tombstoned_at IS NULL;
CREATE INDEX episodes_unconsolidated ON deepresearch.episodes (kind, occurred_at)
  WHERE consolidated_into IS NULL AND tombstoned_at IS NULL;
