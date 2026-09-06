package eliteacore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The catalogue twin of a published agent.
//
// Publish clones the source version inside the caller's OWN schema and stops
// there. The catalogue reads one schema only — the public project's
// (PublicApplications, handler.go). A version published from any other project
// therefore became `status = 'published'` in a schema the catalogue never
// looks at: the Published tab showed it, ELITEA Catalog stayed empty, and
// nothing reported a failure. Publishing worked only when the author already
// stood inside the public project.
//
// The tables were built for the twin: `applications` and `application_versions`
// both carry `shared_owner_id` / `shared_id` with a UNIQUE constraint on the
// pair (001_initial.sql:322 and :344). Nothing in the service wrote them for
// applications. The skill plane already does exactly this
// (internal/api/v2/skillpublish/publish.go userPublish), and this file is the
// application counterpart of it, using the same pair as the origin key:
// shared_owner_id = the SOURCE project id, shared_id = the SOURCE row id.
//
// What the twin does NOT carry is deliberate. Tool and skill attachments are
// keyed by ids that only exist in the source schema — `entity_tool_mapping.
// tool_id` points at that project's `elitea_tools`, `entity_skill_mapping.
// skill_id` at that project's `skills`. Copying the rows across would publish
// dangling ids that resolve to whatever happens to hold the same id in the
// public schema, which is worse than publishing none. The published agent's
// own project keeps its full clone with every attachment; the catalogue entry
// is the stripped snapshot the publish dialog promises ("Internal tool
// endpoints" are among the components it says are removed).

// errCatalogVersionNameTaken reports that the catalogue twin of this agent
// already carries a version of the requested name. It is a caller error, not a
// server fault: the same 422 the source-schema collision answers.
var errCatalogVersionNameTaken = errors.New("catalog version name already exists")

// catalogTwinTargets is what a publish needs to know to mirror one version.
type catalogTwinTargets struct {
	// ApplicationID is the twin application row in the public schema.
	ApplicationID int
	// VersionID is the twin version row in the public schema.
	VersionID int
}

// isPublicProject reports whether projectID already IS the public project, in
// which case the clone Publish made is already in the catalogue's schema and a
// twin would be a duplicate of it.
func isPublicProject(projectID string) bool {
	return strings.TrimSpace(projectID) == publicProjectIDOrDefault()
}

// mirrorPublishedVersion copies a freshly published version into the public
// project's schema so the catalogue can see it.
//
// It runs inside the publish transaction. A twin that fails to write must not
// leave a version the Published tab shows and the catalogue does not, which is
// the exact split this function exists to close.
//
// tx is the publish transaction; sourceSchema and sourceProjectID name the
// author's project; sourceAppID and publishedVersionID are the rows Publish
// just wrote.
func mirrorPublishedVersion(
	ctx context.Context,
	tx pgx.Tx,
	sourceSchema, sourceProjectID string,
	sourceAppID, publishedVersionID int,
	versionName, category string,
) (catalogTwinTargets, error) {
	if isPublicProject(sourceProjectID) {
		return catalogTwinTargets{}, nil
	}
	sourceProjectNumeric, err := strconv.Atoi(strings.TrimSpace(sourceProjectID))
	if err != nil {
		return catalogTwinTargets{}, fmt.Errorf("catalog mirror: project id %q is not numeric: %w", sourceProjectID, err)
	}
	publicSchema := publicTenantSchema()

	// (1) The twin application, created on the first publish and reused after.
	//
	// ON CONFLICT DO UPDATE rather than DO NOTHING: DO NOTHING returns no row,
	// so a second publish of the same agent would have to re-read the id, and
	// the name and description the catalogue shows would freeze at whatever
	// they were on the first publish. The author renaming their agent and
	// publishing again expects the catalogue to follow.
	var (
		name, description string
		icon              *string
		ownerID           int
	)
	if err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT name, COALESCE(description, ''), icon, owner_id FROM %s.applications WHERE id = $1`, sourceSchema),
		sourceAppID,
	).Scan(&name, &description, &icon, &ownerID); err != nil {
		return catalogTwinTargets{}, fmt.Errorf("catalog mirror: source application %d: %w", sourceAppID, err)
	}

	var twinAppID int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.applications (name, description, icon, owner_id, shared_owner_id, shared_id, meta)
		VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb)
		ON CONFLICT ON CONSTRAINT application_shared_origin
		DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, icon = EXCLUDED.icon
		RETURNING id`, publicSchema),
		name, description, icon, ownerID, sourceProjectNumeric, sourceAppID,
	).Scan(&twinAppID); err != nil {
		return catalogTwinTargets{}, fmt.Errorf("catalog mirror: application twin: %w", err)
	}

	// (2) The catalogue version, copied from the clone Publish just made so the
	// meta overlay (source_version_id, category) rides along unchanged.
	//
	// `shared_id` is the CLONE's id, not the draft's: unpublish deletes the
	// twin by that key, and the clone is the row unpublish reverts.
	metaOverlay := fmt.Sprintf(`{"source_project_id": %d, "source_version_id": "%d"}`, sourceProjectNumeric, publishedVersionID)
	if category != "" {
		metaOverlay = fmt.Sprintf(
			`{"source_project_id": %d, "source_version_id": "%d", "category": %s}`,
			sourceProjectNumeric, publishedVersionID, strconv.Quote(category),
		)
	}
	var twinVersionID int
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.application_versions
			(application_id, name, status, author_id, llm_settings, instructions,
			 conversation_starters, welcome_message, agent_type, meta, pipeline_settings,
			 shared_owner_id, shared_id)
		SELECT $2, $3, 'published', author_id, llm_settings, instructions,
			   conversation_starters, welcome_message, agent_type,
			   COALESCE(meta, '{}'::jsonb) || $4::jsonb,
			   pipeline_settings, $5, id
		FROM %s.application_versions WHERE id = $1
		RETURNING id`, publicSchema, sourceSchema),
		publishedVersionID, twinAppID, versionName, metaOverlay, sourceProjectNumeric,
	).Scan(&twinVersionID)
	if err != nil {
		if isCatalogUniqueViolation(err) {
			return catalogTwinTargets{}, errCatalogVersionNameTaken
		}
		return catalogTwinTargets{}, fmt.Errorf("catalog mirror: version twin: %w", err)
	}

	return catalogTwinTargets{ApplicationID: twinAppID, VersionID: twinVersionID}, nil
}

// removeCatalogTwins deletes the catalogue rows a set of source versions
// produced.
//
// Unpublish reverts the source clone to `draft`. Leaving the twin behind would
// keep the agent in ELITEA Catalog after its author took it down, which is the
// one direction of this feature that must never fail quietly — the publish
// dialog tells the author "You retain the ability to unpublish your own agent".
//
// The twin application row is left in place when it still has versions, and
// removed when the last one goes, so a re-publish reuses the same origin key
// instead of accumulating empty catalogue entries.
func removeCatalogTwins(ctx context.Context, ex catalogExecer, sourceProjectID string, sourceVersionIDs []int) error {
	if isPublicProject(sourceProjectID) || len(sourceVersionIDs) == 0 {
		return nil
	}
	sourceProjectNumeric, err := strconv.Atoi(strings.TrimSpace(sourceProjectID))
	if err != nil {
		return fmt.Errorf("catalog mirror: project id %q is not numeric: %w", sourceProjectID, err)
	}
	publicSchema := publicTenantSchema()

	if _, err := ex.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s.application_versions
		WHERE shared_owner_id = $1 AND shared_id = ANY($2::int[])`, publicSchema),
		sourceProjectNumeric, sourceVersionIDs,
	); err != nil {
		return fmt.Errorf("catalog mirror: delete version twins: %w", err)
	}

	if _, err := ex.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s.applications a
		WHERE a.shared_owner_id = $1
		  AND NOT EXISTS (SELECT 1 FROM %s.application_versions v WHERE v.application_id = a.id)`,
		publicSchema, publicSchema), sourceProjectNumeric,
	); err != nil {
		return fmt.Errorf("catalog mirror: delete empty application twins: %w", err)
	}
	return nil
}

// catalogExecer is the narrow write surface removeCatalogTwins needs, so it can
// run on the pool or inside a transaction.
type catalogExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func isCatalogUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "_application_version_name_uc") ||
		strings.Contains(msg, "application_version_shared_origin")
}
