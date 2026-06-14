package store

import (
	"embed"

	"github.com/sirupsen/logrus"
	"github.com/sirus20x6/adamaton-core/pgutil"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate brings the deepresearch schema up to the latest r2g migration.
// Tracks its own version table (schema_migrations_r2g) so it can be
// rolled forward or rolled back independently of the R2R Python
// backend's schema and of other Adamaton stores.
//
// The migrations cover the H-Mem foundation (rollup-tree columns on
// documents, episodes table, entities + bi-temporal entity_relations).
// See r2g/store/migrations/ for the SQL and the umbrella plan
// glowing-humming-treasure.md for the broader phasing.
func Migrate(dsn string, logger *logrus.Logger) error {
	return pgutil.MigrateAll(dsn, "r2g", "migrations", migrationsFS, logger)
}
