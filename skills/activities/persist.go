package activities

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// PersistActivities groups activity methods that touch evo.skills
// directly — the only one in Phase 2A is StampR2RDocumentID, called
// after a successful R2R mirror so the dashboard can deep-link to the
// platform document without polling R2R back.
//
// Mirrors the dependency-injection shape of
// evolve/activities/smoke.go:19-22 (Store + Logger), with pgxpool.Pool
// in place of the evolve store wrapper because the skills worker
// doesn't depend on evolve.
//
// CorpusID is read from SKILLS_R2R_CORPUS_ID at worker startup and
// stamped into evo.skills.r2r_corpus_id when non-empty (matches the
// dashboard's old recordR2RDocID behaviour). Empty value disables the
// corpus-stamp branch — the single-column UPDATE runs instead.
type PersistActivities struct {
	Pool     *pgxpool.Pool
	CorpusID string
	Logger   *logrus.Logger
}

// StampR2RDocumentID updates evo.skills.r2r_document_id to the skill's
// own id (the platform mirror reuses the id as the document id). The
// WHERE clause is idempotent — re-running the activity after a retry
// is a no-op when the row already carries the document id.
//
// When the activity struct was constructed with CorpusID != "" this
// method delegates to StampR2RWithCorpus so r2r_corpus_id is stamped
// in the same UPDATE. The workflow stays single-signature (it always
// calls StampR2RDocumentID) — the corpus decision is configuration,
// not control flow.
//
// SQL mirrored verbatim from
// dashboard/apiserver/skills_endpoints.go:381-394.
func (a *PersistActivities) StampR2RDocumentID(ctx context.Context, skillID string) error {
	if a.CorpusID != "" {
		return a.StampR2RWithCorpus(ctx, skillID, a.CorpusID)
	}
	const q = `
		UPDATE evo.skills SET r2r_document_id = $1
		WHERE id = $1 AND (r2r_document_id IS NULL OR r2r_document_id <> $1)
	`
	if _, err := a.Pool.Exec(ctx, q, skillID); err != nil {
		return err
	}
	if a.Logger != nil {
		a.Logger.WithField("skill_id", skillID).Debug("stamped r2r_document_id")
	}
	return nil
}

// StampR2RWithCorpus is the corpus-aware variant — same intent, but
// also writes r2r_corpus_id. Called when the worker was started with
// SKILLS_R2R_CORPUS_ID set; the workflow itself doesn't know about the
// corpus configuration, so registrations_sync.go decides which method
// to wire as the StampR2RDocumentID activity name at startup.
//
// SQL mirrored verbatim from
// dashboard/apiserver/skills_endpoints.go:381-394 (the corpus branch).
func (a *PersistActivities) StampR2RWithCorpus(ctx context.Context, skillID, corpusID string) error {
	const q = `
		UPDATE evo.skills SET r2r_document_id = $1, r2r_corpus_id = $2
		WHERE id = $1
	`
	if _, err := a.Pool.Exec(ctx, q, skillID, corpusID); err != nil {
		return err
	}
	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"skill_id":  skillID,
			"corpus_id": corpusID,
		}).Debug("stamped r2r_document_id + r2r_corpus_id")
	}
	return nil
}
