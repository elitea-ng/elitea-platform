package main

// The legacy PgVector server (#1359): where a deleted project's project_<id>
// database and project_<id>_user role are dropped from
// (projectprovisioning/legacy_vector_database.go).
//
// TWO VARIABLES, AT MOST ONE SET.
//
//   - ELITEA_LEGACY_PGVECTOR_ADMIN_URL: the admin URL of the server the old
//     per-project store provisioned on (its bootstrap connection: the public
//     project's `elitea-pgvector` configuration in the legacy runtime). A
//     project delete then drops the project's legacy database and role, and
//     `elitea-main legacy-pgvector-orphans` can find the ones older deletes
//     left. A secret: it carries a password.
//   - ELITEA_LEGACY_PGVECTOR_RETIRED=true: the operator's statement that the
//     server is gone with its databases; a delete's drop step closes without a
//     drop.
//
// With neither, a delete whose project shows a legacy database (its
// configuration row, or the database on the platform server) keeps its
// journal row open, logged at Error with both names, rather than completing
// over the data. A deployment that never ran the store sets neither and sees
// no difference. Both set is a contradiction and a boot error, as is an
// unparsable value.

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacypgvector"
)

const (
	legacyPgvectorAdminURLEnv = projectprovisioning.LegacyPgvectorAdminURLEnv // "ELITEA_LEGACY_PGVECTOR_ADMIN_URL"
	legacyPgvectorRetiredEnv  = projectprovisioning.LegacyPgvectorRetiredEnv  // "ELITEA_LEGACY_PGVECTOR_RETIRED"
)

// composeLegacyPgvector reads the two variables. It returns the dropper (nil
// without an admin URL) and the retired switch.
func composeLegacyPgvector(logger *slog.Logger, lookup func(string) (string, bool)) (*legacypgvector.Dropper, bool, error) {
	rawURL, _ := lookup(legacyPgvectorAdminURLEnv)
	rawURL = strings.TrimSpace(rawURL)
	rawRetired, _ := lookup(legacyPgvectorRetiredEnv)
	rawRetired = strings.TrimSpace(rawRetired)
	retired := false
	if rawRetired != "" {
		parsed, err := strconv.ParseBool(rawRetired)
		if err != nil {
			return nil, false, fmt.Errorf("%s=%q is not a boolean (true/false)", legacyPgvectorRetiredEnv, rawRetired)
		}
		retired = parsed
	}
	if rawURL == "" {
		if retired {
			logger.Warn("the legacy PgVector server is declared RETIRED: a deleted project's legacy database step closes without a drop",
				"set", legacyPgvectorRetiredEnv)
		}
		return nil, retired, nil
	}
	if retired {
		return nil, false, fmt.Errorf("%s=true while %s is set: a server that is dialled is not retired. Clear one of them",
			legacyPgvectorRetiredEnv, legacyPgvectorAdminURLEnv)
	}
	dropper, err := legacypgvector.NewDropper(rawURL)
	if err != nil {
		// Never the URL: it carries a password.
		if errors.Is(err, legacypgvector.ErrInvalidAdminURL) {
			return nil, false, fmt.Errorf("%s: %w", legacyPgvectorAdminURLEnv, err)
		}
		return nil, false, err
	}
	logger.Info("legacy PgVector server is wired: a deleted project's project_<id> database and role are dropped",
		"admin_host", dropper.AdminHost(), "admin_database", dropper.AdminDatabase())
	return dropper, false, nil
}
