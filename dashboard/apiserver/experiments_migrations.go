package apiserver

import (
	"embed"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

//go:embed migrations/*.sql
var experimentsMigrationsFS embed.FS

// runExperimentsMigrations applies the platform.experiments / experiment_metrics
// migrations against dsn. Boot-time best-effort: a failure logs a warning and
// leaves the page degraded (writes 503, reads still served by PostgREST so
// long as the tables exist) rather than panicking the apiserver.
//
// The migrations table is schema_migrations_experiments, namespaced per
// pgutil convention so it doesn't collide with evo_datasets'
// schema_migrations_datasets.
func runExperimentsMigrations(dsn string, logger *logrus.Logger) {
	if dsn == "" {
		logger.Warn("experiments migrations skipped: no Postgres DSN configured")
		return
	}
	if err := pgutil.MigrateAll(dsn, "experiments", "migrations", experimentsMigrationsFS, logger); err != nil {
		logger.WithError(err).Warn("experiments migrations failed; /platform/experiments writes will 503")
		return
	}
}
