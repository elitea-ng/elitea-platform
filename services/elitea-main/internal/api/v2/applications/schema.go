package applications

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// defaultVersionName is the well-known "unnamed default" version name shared
// with the UI (apps/elitea-web/src/entities/version/model/selectors.ts's
// LATEST_VERSION_NAME) and with the repository's own CreateVersion default.
const defaultVersionName = "base"

// defaultVersionMetaKey is the applications.meta key recording which version is
// the application's default. application_versions has no is_default column —
// the fact lives on the owning applications row, and both this package's Get
// and repos/applications.go's scanVersion derive the per-version flag from it.
// Kept in step with repos.defaultVersionMetaKey, which writes it.
const defaultVersionMetaKey = "default_version_id"

// tenantSchema returns the project's tenant schema as a fully quoted
// PostgreSQL identifier (e.g. `"p_1"`), ready to interpolate with %s, and
// false when projectID — a raw chi.URLParam — is not a plain decimal id.
//
// The handlers previously built the schema with fmt.Sprintf("p_%s", projectID)
// and interpolated it with %q. %q is a Go string quoter, not a PostgreSQL
// identifier quoter: it writes an embedded quote as \" where PostgreSQL wants
// it doubled, and PostgreSQL treats the backslash as an ordinary character and
// ENDS the identifier at that quote, so the rest of the caller's text became
// SQL. The validation and the quoting both live in one place now; see
// internal/infra/db/tenantschema and issue #543.
func tenantSchema(projectID string) (string, bool) {
	quoted, err := tenantschema.Quote(projectID)
	if err != nil {
		return "", false
	}
	return quoted, true
}
