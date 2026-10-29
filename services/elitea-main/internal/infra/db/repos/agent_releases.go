package repos

// Agent releases (ADR-0040 decision 2, issue #1492, shared 0198).
//
// Publish records a release in the publish transaction (RecordPublishedTx):
// the published version's canonical definition with every toolkit's
// settings removed, its definition digest (#1462) and one pin per toolkit.
// Unpublish withdraws the releases of the reverted versions in the
// unpublish transaction (WithdrawVersionsTx).
// Rollback re-points the application's published release at an earlier one
// (Rollback).
//
// A run resolves its release inside the start resolver's transaction
// (resolveRunRelease; its comment has the rules): a run that names a
// release, or a run of a published version that has one. The release's
// pins are checked against the live toolkit and configuration rows. When
// the version row is no longer the release (it was edited after an
// unpublish, or the published release was rolled back to an earlier one),
// the run gets the release's definition, with each toolkit's live settings
// put back once their digests match the pins, and the result must hash to
// the release's digest.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/publicproject"
)

// maxReleaseListRows bounds one release or revision listing.
const maxReleaseListRows = 500

// AgentReleases stores agent releases and reads draft revisions.
type AgentReleases struct {
	projects projectStore
	// publicProjectID resolves the catalogue project, the fallback of a
	// non-private configuration reference (configurations expansion).
	publicProjectID func() int64
}

// NewAgentReleases builds the store over pool.
func NewAgentReleases(pool *pgxpool.Pool) (*AgentReleases, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return &AgentReleases{projects: projects, publicProjectID: func() int64 { return int64(publicproject.ID()) }}, nil
}

// RecordReleaseRequest names the version a publish created.
type RecordReleaseRequest struct {
	ProjectID       int64
	ApplicationID   int64
	VersionID       int64
	SourceVersionID int64
	Name            string
	ActorID         int64
}

// RecordRelease snapshots the published version and points the
// application's published release at it, in one transaction. Recording the
// same version twice answers the release already recorded.
func (s *AgentReleases) RecordRelease(ctx context.Context, request RecordReleaseRequest) (agentrelease.Release, error) {
	if s == nil || s.projects == nil {
		return agentrelease.Release{}, errors.New("agent releases: no database")
	}
	var recorded agentrelease.Release
	err := s.projects.WithinProjectTx(ctx, request.ProjectID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx sqlExecutor) error {
		var err error
		recorded, err = s.recordRelease(ctx, tx, request)
		return err
	})
	if err != nil {
		return agentrelease.Release{}, err
	}
	return recorded, nil
}

// RecordPublishedTx is RecordRelease inside the publish handler's own
// transaction (eliteacore.ReleaseRecorder): the release commits with the
// publish or not at all. It binds the transaction to the project's schema
// (a transaction-local search path) before it reads the version.
func (s *AgentReleases) RecordPublishedTx(ctx context.Context, tx pgx.Tx, projectID, applicationID, versionID, sourceVersionID int64, name string, actorID int64) (int64, error) {
	if s == nil {
		return 0, errors.New("agent releases: no database")
	}
	if err := tenant.BindProject(ctx, tx, tenant.Project{ID: projectID}); err != nil {
		return 0, fmt.Errorf("agent releases: %w", err)
	}
	release, err := s.recordRelease(ctx, pgxExecutor{queryer: tx}, RecordReleaseRequest{
		ProjectID: projectID, ApplicationID: applicationID, VersionID: versionID,
		SourceVersionID: sourceVersionID, Name: name, ActorID: actorID,
	})
	if err != nil {
		return 0, err
	}
	return release.ID, nil
}

// recordRelease is RecordRelease's body in the caller's transaction.
func (s *AgentReleases) recordRelease(ctx context.Context, tx sqlExecutor, request RecordReleaseRequest) (agentrelease.Release, error) {
	application, applicationOK := currentAgentDatabaseID(request.ApplicationID)
	version, versionOK := currentAgentDatabaseID(request.VersionID)
	if request.ProjectID <= 0 || !applicationOK || !versionOK || strings.TrimSpace(request.Name) == "" {
		return agentrelease.Release{}, errors.New("agent releases: invalid release request")
	}
	var recorded agentrelease.Release
	err := func() error {
		// The pinned toolkit rows are read twice (the projection, then the
		// pins): share-lock them first, so an edit cannot land between the
		// two reads in a READ COMMITTED publish transaction.
		if err := lockVersionToolkits(ctx, tx, request.ProjectID, request.ApplicationID, request.VersionID); err != nil {
			return err
		}
		queries, ok := tx.(currentApplicationVersionQuerier)
		if !ok {
			return errors.New("agent releases: the version query is unavailable")
		}
		row, err := queries.ResolveCurrentApplicationVersionDetails(ctx, sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: version, ApplicationID: application,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return agentrelease.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("agent releases: read the version definition: %w", err)
		}
		projection := []byte(row.ApplicationVersionDetailsJson)
		digest, err := agentidentity.VersionDefinitionDigest(request.ProjectID, request.ApplicationID, request.VersionID, projection)
		if err != nil {
			return fmt.Errorf("agent releases: digest the version definition: %w", err)
		}
		definition, projected, err := agentrelease.SplitDefinition(projection)
		if err != nil {
			return err
		}
		pins, err := s.pinToolkits(ctx, tx, request.ProjectID, request.ApplicationID, request.VersionID, definition, projected)
		if err != nil {
			return err
		}
		encodedDefinition, err := agentrelease.Encode(definition)
		if err != nil {
			return err
		}
		encodedPins, err := json.Marshal(pins)
		if err != nil {
			return err
		}
		var id int64
		err = tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.application_releases
	(tenant_project_id, application_id, application_version_id, source_version_id, name,
	 definition_digest, definition, toolkit_pins, published_by)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9)
ON CONFLICT (tenant_project_id, definition_digest) DO NOTHING
RETURNING id`,
			request.ProjectID, request.ApplicationID, request.VersionID, nullablePositive(request.SourceVersionID),
			request.Name, digest[:], string(encodedDefinition), string(encodedPins), nullablePositive(request.ActorID),
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Recorded before (a retried publish): answer that release.
			if err := tx.QueryRow(ctx, `
SELECT id FROM elitea_lifecycle.application_releases
WHERE tenant_project_id = $1 AND definition_digest = $2`, request.ProjectID, digest[:]).Scan(&id); err != nil {
				return fmt.Errorf("agent releases: read the recorded release: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("agent releases: insert the release: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_lifecycle.application_release_heads
	(tenant_project_id, application_id, release_id, pointed_by, cause)
VALUES ($1, $2, $3, $4, 'publish')
ON CONFLICT (tenant_project_id, application_id) DO UPDATE
SET release_id = EXCLUDED.release_id, pointed_by = EXCLUDED.pointed_by,
    pointed_at = clock_timestamp(), cause = 'publish', reason = ''`,
			request.ProjectID, request.ApplicationID, id, nullablePositive(request.ActorID)); err != nil {
			return fmt.Errorf("agent releases: point the published release: %w", err)
		}
		recorded, err = readRelease(ctx, tx, request.ProjectID, request.ApplicationID, id, true)
		return err
	}()
	if err != nil {
		return agentrelease.Release{}, err
	}
	return recorded, nil
}

// lockVersionToolkits share-locks the toolkit rows attached to a version.
func lockVersionToolkits(ctx context.Context, tx sqlExecutor, projectID, applicationID, versionID int64) error {
	schemaName, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT tool.id
FROM %[1]s.entity_tool_mapping AS mapping
JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_version_id = $1 AND mapping.entity_id = $2 AND mapping.entity_type = 'agent'
ORDER BY tool.id
FOR SHARE OF tool`, schemaName), versionID, applicationID)
	if err != nil {
		return fmt.Errorf("agent releases: lock the toolkit attachments: %w", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent releases: lock the toolkit attachments: %w", err)
	}
	return nil
}

// releaseToolkitRow is one toolkit attachment of a version, in the order
// the definition projection lists them.
type releaseToolkitRow struct {
	toolkitID     int64
	toolkitType   string
	settings      []byte
	selectedTools []byte
}

// readVersionToolkits reads the toolkit attachments of a version with the
// projection's own join, filter and order (agent_chat.sql `tools`).
func readVersionToolkits(ctx context.Context, tx sqlExecutor, schemaName string, applicationID, versionID int64) ([]releaseToolkitRow, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT tool.id, tool.type, COALESCE(tool.settings::text, 'null'), COALESCE(mapping.selected_tools::text, 'null')
FROM %[1]s.entity_tool_mapping AS mapping
JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_version_id = $1 AND mapping.entity_id = $2 AND mapping.entity_type = 'agent'
ORDER BY mapping.id`, schemaName), versionID, applicationID)
	if err != nil {
		return nil, fmt.Errorf("agent releases: read the toolkit attachments: %w", err)
	}
	defer rows.Close()
	var toolkits []releaseToolkitRow
	for rows.Next() {
		var (
			row                     releaseToolkitRow
			settings, selectedTools string
		)
		if err := rows.Scan(&row.toolkitID, &row.toolkitType, &settings, &selectedTools); err != nil {
			return nil, fmt.Errorf("agent releases: read a toolkit attachment: %w", err)
		}
		row.settings, row.selectedTools = []byte(settings), []byte(selectedTools)
		toolkits = append(toolkits, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent releases: read the toolkit attachments: %w", err)
	}
	return toolkits, nil
}

// pinToolkits pins every toolkit of the definition: digests of its stored
// and projected settings, the agent's selection, the configurations its
// references resolve to and its credential reference ids.
func (s *AgentReleases) pinToolkits(ctx context.Context, tx sqlExecutor, projectID, applicationID, versionID int64, definition map[string]any, projected []map[string]any) ([]agentrelease.ToolkitPin, error) {
	schemaName, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	toolkits, err := readVersionToolkits(ctx, tx, schemaName, applicationID, versionID)
	if err != nil {
		return nil, err
	}
	if len(toolkits) != agentrelease.ToolCount(definition) {
		return nil, errors.New("agent releases: the toolkit attachments changed while the release was recorded")
	}
	references := make([][]agentrelease.ConfigurationReference, len(toolkits))
	var titles []string
	for index, toolkit := range toolkits {
		stored, _ := agentrelease.DecodeObject(toolkit.settings)
		references[index] = agentrelease.ConfigurationReferences(stored)
		titles = appendSharedTitles(titles, references[index])
	}
	configurations, err := s.lookupConfigurations(ctx, tx, projectID, titles)
	if err != nil {
		return nil, err
	}
	pins := make([]agentrelease.ToolkitPin, 0, len(toolkits))
	for index, toolkit := range toolkits {
		if id, ok := agentrelease.ToolkitID(definition, index); !ok || id != toolkit.toolkitID {
			return nil, errors.New("agent releases: the toolkit attachments changed while the release was recorded")
		}
		storedSHA, err := agentrelease.CanonicalSHA256(toolkit.settings)
		if err != nil {
			return nil, fmt.Errorf("agent releases: digest the settings of toolkit %d: %w", toolkit.toolkitID, err)
		}
		var projectedValue any
		if projected[index] != nil {
			projectedValue = projected[index]
		}
		projectedSHA, err := agentrelease.ValueSHA256(projectedValue)
		if err != nil {
			return nil, fmt.Errorf("agent releases: digest the projected settings of toolkit %d: %w", toolkit.toolkitID, err)
		}
		selected, err := agentrelease.CanonicalJSON(toolkit.selectedTools)
		if err != nil {
			return nil, fmt.Errorf("agent releases: read the selection of toolkit %d: %w", toolkit.toolkitID, err)
		}
		pin := agentrelease.ToolkitPin{
			Index: index, ToolkitID: toolkit.toolkitID, Type: toolkit.toolkitType,
			SettingsSHA256: storedSHA, ProjectedSettingsSHA256: projectedSHA,
			SelectedTools:          selected,
			Configurations:         []agentrelease.ConfigurationPin{},
			CredentialReferenceIDs: []string{},
		}
		for _, reference := range references[index] {
			configuration := agentrelease.ConfigurationPin{
				Path: reference.Path, Title: reference.Title, Private: reference.Private,
				CredentialReferenceID: reference.CredentialReferenceID,
			}
			if !reference.Private {
				found := configurations[reference.Title]
				configuration.ConfigurationUUID = found.uuid
				configuration.ProjectID = found.projectID
				configuration.DataSHA256 = found.dataSHA256
			}
			pin.Configurations = append(pin.Configurations, configuration)
			if reference.CredentialReferenceID != "" && !containsText(pin.CredentialReferenceIDs, reference.CredentialReferenceID) {
				pin.CredentialReferenceIDs = append(pin.CredentialReferenceIDs, reference.CredentialReferenceID)
			}
		}
		sort.Strings(pin.CredentialReferenceIDs)
		pins = append(pins, pin)
	}
	return pins, nil
}

// releaseConfiguration is the configuration row a non-private reference
// resolves to; uuid is empty when it resolves to none.
type releaseConfiguration struct {
	uuid       string
	projectID  int64
	dataSHA256 string
}

// appendSharedTitles adds the titles of the non-private references to
// titles, each once.
func appendSharedTitles(titles []string, references []agentrelease.ConfigurationReference) []string {
	for _, reference := range references {
		if !reference.Private && !containsText(titles, reference.Title) {
			titles = append(titles, reference.Title)
		}
	}
	return titles
}

// lookupConfigurations resolves non-private configuration references by
// title the way the configurations expansion does: the run's project
// first, then a shared row of the public project. A title that resolves
// to no row is absent from the answer (its zero value has no uuid). It
// sends at most two statements per schema, whatever the number of titles.
func (s *AgentReleases) lookupConfigurations(ctx context.Context, tx sqlExecutor, projectID int64, titles []string) (map[string]releaseConfiguration, error) {
	found := map[string]releaseConfiguration{}
	if len(titles) == 0 {
		return found, nil
	}
	if err := readConfigurationsByTitle(ctx, tx, projectID, titles, false, found); err != nil {
		return nil, err
	}
	public := int64(0)
	if s.publicProjectID != nil {
		public = s.publicProjectID()
	}
	if public <= 0 || public == projectID {
		return found, nil
	}
	var missing []string
	for _, title := range titles {
		if _, ok := found[title]; !ok {
			missing = append(missing, title)
		}
	}
	if len(missing) == 0 {
		return found, nil
	}
	if err := readConfigurationsByTitle(ctx, tx, public, missing, true, found); err != nil {
		return nil, err
	}
	return found, nil
}

// readConfigurationsByTitle is FindCurrentConfigurationByEliteaTitle for
// several titles against one named project schema, one row per title,
// written into found. A project with no configuration table holds no
// configuration.
func readConfigurationsByTitle(ctx context.Context, tx sqlExecutor, projectID int64, titles []string, sharedOnly bool, found map[string]releaseConfiguration) error {
	schemaName, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schemaName+".configuration").Scan(&exists); err != nil {
		return fmt.Errorf("agent releases: find the configuration table: %w", err)
	}
	if !exists {
		return nil
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT ON (elitea_title) elitea_title, uuid::text, project_id, COALESCE(data::text, 'null')
FROM %s.configuration
WHERE project_id = $1 AND elitea_title = ANY($2::text[]) AND (NOT $3::boolean OR shared = true)
ORDER BY elitea_title`, schemaName), projectID, titles, sharedOnly)
	if err != nil {
		return fmt.Errorf("agent releases: read the configurations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			title, data string
			row         releaseConfiguration
		)
		if err := rows.Scan(&title, &row.uuid, &row.projectID, &data); err != nil {
			return fmt.Errorf("agent releases: read a configuration: %w", err)
		}
		if row.dataSHA256, err = agentrelease.CanonicalSHA256([]byte(data)); err != nil {
			return fmt.Errorf("agent releases: digest a configuration: %w", err)
		}
		found[title] = row
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent releases: read the configurations: %w", err)
	}
	return nil
}

// releaseColumns is the projection of one release row; `r` is the release,
// `h` the application's head row (LEFT JOIN).
const releaseColumns = `r.id, r.tenant_project_id, r.application_id, r.application_version_id,
	COALESCE(r.source_version_id, 0), r.name, r.definition_digest, r.state,
	COALESCE(r.published_by, 0), r.created_at, r.withdrawn_at, COALESCE(r.withdrawn_by, 0),
	(h.release_id IS NOT NULL)`

const releaseFrom = `FROM elitea_lifecycle.application_releases AS r
LEFT JOIN elitea_lifecycle.application_release_heads AS h
  ON h.tenant_project_id = r.tenant_project_id AND h.application_id = r.application_id
 AND h.release_id = r.id`

func scanRelease(row sqlRow, detail bool) (agentrelease.Release, error) {
	var (
		release agentrelease.Release
		digest  []byte
		pins    string
		def     string
	)
	dest := []any{
		&release.ID, &release.ProjectID, &release.ApplicationID, &release.ApplicationVersionID,
		&release.SourceVersionID, &release.Name, &digest, &release.State,
		&release.PublishedBy, &release.CreatedAt, &release.WithdrawnAt, &release.WithdrawnBy,
		&release.IsHead,
	}
	if detail {
		dest = append(dest, &def, &pins)
	}
	if err := row.Scan(dest...); err != nil {
		return agentrelease.Release{}, err
	}
	if len(digest) != len(release.DefinitionDigest) {
		return agentrelease.Release{}, errors.New("agent releases: a stored digest is malformed")
	}
	copy(release.DefinitionDigest[:], digest)
	if detail {
		release.Definition = json.RawMessage(def)
		if err := json.Unmarshal([]byte(pins), &release.ToolkitPins); err != nil {
			return agentrelease.Release{}, fmt.Errorf("agent releases: decode the toolkit pins: %w", err)
		}
	}
	return release, nil
}

// readRelease reads one release of an application; detail adds the
// definition and the pins.
func readRelease(ctx context.Context, tx sqlExecutor, projectID, applicationID, releaseID int64, detail bool) (agentrelease.Release, error) {
	columns := releaseColumns
	if detail {
		columns += `, r.definition::text, r.toolkit_pins::text`
	}
	release, err := scanRelease(tx.QueryRow(ctx, `SELECT `+columns+` `+releaseFrom+`
WHERE r.tenant_project_id = $1 AND r.application_id = $2 AND r.id = $3`,
		projectID, applicationID, releaseID), detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentrelease.Release{}, agentrelease.ErrNotFound
	}
	if err != nil {
		return agentrelease.Release{}, fmt.Errorf("agent releases: read a release: %w", err)
	}
	return release, nil
}

// WithdrawVersions marks the releases of the unpublished versions withdrawn
// and moves each affected application's published release to its newest
// release still published, or removes it when none is.
func (s *AgentReleases) WithdrawVersions(ctx context.Context, projectID int64, versionIDs []int64, actorID int64) error {
	if s == nil || s.projects == nil {
		return errors.New("agent releases: no database")
	}
	if projectID <= 0 || len(versionIDs) == 0 {
		return nil
	}
	return s.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{}, func(tx sqlExecutor) error {
		return withdrawVersions(ctx, tx, projectID, versionIDs, actorID)
	})
}

// WithdrawVersionsTx is WithdrawVersions inside the unpublish handler's
// own transaction (eliteacore.ReleaseRecorder): the withdrawal commits with
// the revert to draft or not at all.
func (s *AgentReleases) WithdrawVersionsTx(ctx context.Context, tx pgx.Tx, projectID int64, versionIDs []int64, actorID int64) error {
	if s == nil {
		return errors.New("agent releases: no database")
	}
	if projectID <= 0 || len(versionIDs) == 0 {
		return nil
	}
	return withdrawVersions(ctx, pgxExecutor{queryer: tx}, projectID, versionIDs, actorID)
}

// withdrawVersions is WithdrawVersions' body in the caller's transaction.
func withdrawVersions(ctx context.Context, tx sqlExecutor, projectID int64, versionIDs []int64, actorID int64) error {
	rows, err := tx.Query(ctx, `
UPDATE elitea_lifecycle.application_releases
SET state = 'withdrawn', withdrawn_at = clock_timestamp(), withdrawn_by = $3
WHERE tenant_project_id = $1 AND application_version_id = ANY($2::bigint[]) AND state = 'published'
RETURNING application_id`, projectID, versionIDs, nullablePositive(actorID))
	if err != nil {
		return fmt.Errorf("agent releases: withdraw: %w", err)
	}
	applications := map[int64]bool{}
	for rows.Next() {
		var application int64
		if err := rows.Scan(&application); err != nil {
			rows.Close()
			return fmt.Errorf("agent releases: withdraw: %w", err)
		}
		applications[application] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent releases: withdraw: %w", err)
	}
	for application := range applications {
		if err := repointAfterWithdrawal(ctx, tx, projectID, application, actorID); err != nil {
			return err
		}
	}
	return nil
}

// repointAfterWithdrawal keeps the published release pointing at a release
// still published: the current one when it still is, else the newest one,
// else none.
func repointAfterWithdrawal(ctx context.Context, tx sqlExecutor, projectID, applicationID, actorID int64) error {
	var headState string
	err := tx.QueryRow(ctx, `
SELECT r.state
FROM elitea_lifecycle.application_release_heads AS h
JOIN elitea_lifecycle.application_releases AS r ON r.id = h.release_id
WHERE h.tenant_project_id = $1 AND h.application_id = $2
FOR UPDATE OF h`, projectID, applicationID).Scan(&headState)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && headState == agentrelease.StatePublished) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent releases: read the published release: %w", err)
	}
	var newest int64
	err = tx.QueryRow(ctx, `
SELECT id FROM elitea_lifecycle.application_releases
WHERE tenant_project_id = $1 AND application_id = $2 AND state = 'published'
ORDER BY id DESC LIMIT 1`, projectID, applicationID).Scan(&newest)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
DELETE FROM elitea_lifecycle.application_release_heads
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID); err != nil {
			return fmt.Errorf("agent releases: clear the published release: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent releases: find a published release: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.application_release_heads
SET release_id = $3, pointed_by = $4, pointed_at = clock_timestamp(), cause = 'unpublish', reason = ''
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID, newest, nullablePositive(actorID)); err != nil {
		return fmt.Errorf("agent releases: move the published release: %w", err)
	}
	return nil
}

// ListReleases lists the releases of an application, newest first.
func (s *AgentReleases) ListReleases(ctx context.Context, projectID, applicationID int64) ([]agentrelease.Release, error) {
	if s == nil || s.projects == nil {
		return nil, errors.New("agent releases: no database")
	}
	releases := []agentrelease.Release{}
	err := s.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		rows, err := tx.Query(ctx, `SELECT `+releaseColumns+` `+releaseFrom+`
WHERE r.tenant_project_id = $1 AND r.application_id = $2
ORDER BY r.id DESC
LIMIT $3`, projectID, applicationID, maxReleaseListRows)
		if err != nil {
			return fmt.Errorf("agent releases: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			release, err := scanRelease(rows, false)
			if err != nil {
				return fmt.Errorf("agent releases: list: %w", err)
			}
			releases = append(releases, release)
		}
		return rows.Err()
	})
	return releases, err
}

// ReleaseDetail is one release with its diff against the release before it.
type ReleaseDetail struct {
	Release agentrelease.Release
	// PreviousID is the application's release before this one; zero for
	// the first. Changes is the diff of the two definitions (toolkit
	// settings are not in a definition; a changed toolkit shows as a
	// changed pin).
	PreviousID int64
	Changes    []agentrelease.Change
}

// GetRelease reads one release with its definition, pins and diff.
func (s *AgentReleases) GetRelease(ctx context.Context, projectID, applicationID, releaseID int64) (ReleaseDetail, error) {
	if s == nil || s.projects == nil {
		return ReleaseDetail{}, errors.New("agent releases: no database")
	}
	var detail ReleaseDetail
	err := s.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		release, err := readRelease(ctx, tx, projectID, applicationID, releaseID, true)
		if err != nil {
			return err
		}
		detail.Release = release
		var previousID int64
		err = tx.QueryRow(ctx, `
SELECT id FROM elitea_lifecycle.application_releases
WHERE tenant_project_id = $1 AND application_id = $2 AND id < $3
ORDER BY id DESC LIMIT 1`, projectID, applicationID, releaseID).Scan(&previousID)
		if errors.Is(err, pgx.ErrNoRows) {
			detail.Changes = []agentrelease.Change{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("agent releases: find the previous release: %w", err)
		}
		previous, err := readRelease(ctx, tx, projectID, applicationID, previousID, true)
		if err != nil {
			return err
		}
		detail.PreviousID = previousID
		detail.Changes, err = releaseChanges(previous, release)
		return err
	})
	return detail, err
}

// releaseChanges diffs two releases: their definitions and their pins.
func releaseChanges(before, after agentrelease.Release) ([]agentrelease.Change, error) {
	left, err := releaseDocument(before)
	if err != nil {
		return nil, err
	}
	right, err := releaseDocument(after)
	if err != nil {
		return nil, err
	}
	return agentrelease.Diff(left, right), nil
}

func releaseDocument(release agentrelease.Release) (map[string]any, error) {
	definition, err := agentrelease.DecodeObject(release.Definition)
	if err != nil {
		return nil, agentrelease.ErrDefinitionUnreadable
	}
	// The version row's identity differs between two releases by design;
	// it is not a change of the agent.
	for _, key := range []string{"id", "created_at", "status", "name"} {
		delete(definition, key)
	}
	encodedPins, err := json.Marshal(release.ToolkitPins)
	if err != nil {
		return nil, err
	}
	pins, err := decodeNumbersValue(encodedPins)
	if err != nil {
		return nil, err
	}
	return map[string]any{"definition": definition, "toolkit_pins": pins}, nil
}

func decodeNumbersValue(raw []byte) (any, error) {
	wrapped, err := agentrelease.DecodeObject(append(append([]byte(`{"v":`), raw...), '}'))
	if err != nil {
		return nil, err
	}
	return wrapped["v"], nil
}

// RollbackOutcome is the published release before and after a rollback.
type RollbackOutcome struct {
	From agentrelease.Release
	To   agentrelease.Release
}

// Rollback re-points the application's published release at releaseID, an
// earlier release of the same application. The application must have a
// published release, and the target's toolkit pins must still match: a
// drifted release is refused, so a rollback never leaves the agent unable
// to run.
func (s *AgentReleases) Rollback(ctx context.Context, projectID, applicationID, releaseID, actorID int64, reason string) (RollbackOutcome, error) {
	if s == nil || s.projects == nil {
		return RollbackOutcome{}, errors.New("agent releases: no database")
	}
	var outcome RollbackOutcome
	err := s.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{}, func(tx sqlExecutor) error {
		var headID int64
		err := tx.QueryRow(ctx, `
SELECT release_id FROM elitea_lifecycle.application_release_heads
WHERE tenant_project_id = $1 AND application_id = $2
FOR UPDATE`, projectID, applicationID).Scan(&headID)
		if errors.Is(err, pgx.ErrNoRows) {
			return agentrelease.ErrNotPublished
		}
		if err != nil {
			return fmt.Errorf("agent releases: read the published release: %w", err)
		}
		target, err := readRelease(ctx, tx, projectID, applicationID, releaseID, true)
		if err != nil {
			return err
		}
		if headID == releaseID {
			return agentrelease.ErrAlreadyHead
		}
		if target.State != agentrelease.StatePublished {
			return agentrelease.ErrWithdrawn
		}
		from, err := readRelease(ctx, tx, projectID, applicationID, headID, false)
		if err != nil {
			return err
		}
		if _, err := reconstructRelease(ctx, tx, s.publicProjectID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.application_release_heads
SET release_id = $3, pointed_by = $4, pointed_at = clock_timestamp(), cause = 'rollback', reason = $5
WHERE tenant_project_id = $1 AND application_id = $2`,
			projectID, applicationID, releaseID, nullablePositive(actorID), reason); err != nil {
			return fmt.Errorf("agent releases: re-point the published release: %w", err)
		}
		target.IsHead, from.IsHead = true, false
		target.Definition, target.ToolkitPins = nil, nil
		outcome = RollbackOutcome{From: from, To: target}
		return nil
	})
	return outcome, err
}

// ListRevisions lists the revisions of one version, newest first.
func (s *AgentReleases) ListRevisions(ctx context.Context, projectID, applicationID, versionID int64) ([]agentrelease.Revision, error) {
	if s == nil || s.projects == nil {
		return nil, errors.New("agent releases: no database")
	}
	revisions := []agentrelease.Revision{}
	err := s.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		rows, err := tx.Query(ctx, `
SELECT id, tenant_project_id, application_id, application_version_id, revision, kind,
       COALESCE(actor_id, 0), created_at, snapshot::text, diff::text
FROM elitea_lifecycle.version_revisions
WHERE tenant_project_id = $1 AND application_id = $2 AND application_version_id = $3
ORDER BY revision DESC
LIMIT $4`, projectID, applicationID, versionID, maxReleaseListRows)
		if err != nil {
			return fmt.Errorf("version revisions: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				revision       agentrelease.Revision
				snapshot, diff string
			)
			if err := rows.Scan(&revision.ID, &revision.ProjectID, &revision.ApplicationID, &revision.ApplicationVersionID,
				&revision.Revision, &revision.Kind, &revision.ActorID, &revision.CreatedAt, &snapshot, &diff); err != nil {
				return fmt.Errorf("version revisions: list: %w", err)
			}
			revision.Snapshot, revision.Diff = json.RawMessage(snapshot), json.RawMessage(diff)
			revisions = append(revisions, revision)
		}
		return rows.Err()
	})
	return revisions, err
}

// resolveRunRelease resolves the release a start runs, inside the start
// resolver's transaction (CurrentAgentStartRepository).
//
//   - explicitID > 0: that release of the application, published or
//     withdrawn; another application's release is ErrNotFound. A pin that
//     no longer matches refuses the run (DriftError). A withdrawn release
//     whose sub-agent toolkit row is gone is
//     ErrWithdrawnDependenciesRemoved: the unpublish deleted the embedded
//     sub-agent copies the publish made, and their toolkit rows with them
//     (eliteacore deleteEmbeddedSubAgents).
//   - otherwise, for a version whose status is `published`: its newest
//     published release R. When the application's published release was
//     rolled back to a release older than R, the run executes that older
//     release, and a pin that no longer matches refuses the run. Else the
//     run executes R when the version row still is R and every pin
//     matches. When a toolkit or configuration of R was edited after the
//     publish, the run executes the version as it stands and records no
//     release: toolkits are shared and edited in place, and refusing every
//     run of the current published agent for that would stop it.
//   - otherwise no release: the run executes the version as it stands.
//
// details is nil when the run executes the version row as read; otherwise
// it is the release's definition (reconstructRelease).
func resolveRunRelease(ctx context.Context, tx sqlExecutor, publicProjectID func() int64, projectID, applicationID, versionID int64, liveProjection []byte, explicitID int64) (*agentexecutionapp.ReleaseRun, []byte, error) {
	if explicitID > 0 {
		release, err := readRelease(ctx, tx, projectID, applicationID, explicitID, true)
		if err != nil {
			return nil, nil, err
		}
		run := &agentexecutionapp.ReleaseRun{ID: release.ID, ApplicationVersionID: release.ApplicationVersionID, DefinitionDigest: release.DefinitionDigest}
		if liveIsRelease(projectID, applicationID, versionID, liveProjection, release) {
			if err := verifyReleasePins(ctx, tx, publicProjectID, projectID, release.ToolkitPins, nil); err != nil {
				return nil, nil, withdrawnRefusal(release, err)
			}
			return run, nil, nil
		}
		details, err := reconstructRelease(ctx, tx, publicProjectID, release)
		if err != nil {
			return nil, nil, withdrawnRefusal(release, err)
		}
		return run, details, nil
	}
	var status struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(liveProjection, &status) != nil || status.Status != agentrelease.StatePublished {
		return nil, nil, nil
	}
	var (
		ownID  int64
		headID *int64
	)
	err := tx.QueryRow(ctx, `
SELECT r.id, h.release_id
FROM elitea_lifecycle.application_releases AS r
LEFT JOIN elitea_lifecycle.application_release_heads AS h
  ON h.tenant_project_id = r.tenant_project_id AND h.application_id = r.application_id
WHERE r.tenant_project_id = $1 AND r.application_id = $2 AND r.application_version_id = $3
  AND r.state = 'published'
ORDER BY r.id DESC
LIMIT 1`, projectID, applicationID, versionID).Scan(&ownID, &headID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A version published before releases existed.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("agent releases: find the run's release: %w", err)
	}
	if headID != nil && *headID < ownID {
		// Rolled back to an earlier release: run that release.
		release, err := readRelease(ctx, tx, projectID, applicationID, *headID, true)
		if err != nil {
			return nil, nil, err
		}
		details, err := reconstructRelease(ctx, tx, publicProjectID, release)
		if err != nil {
			return nil, nil, err
		}
		return &agentexecutionapp.ReleaseRun{ID: release.ID, ApplicationVersionID: release.ApplicationVersionID, DefinitionDigest: release.DefinitionDigest}, details, nil
	}
	release, err := readRelease(ctx, tx, projectID, applicationID, ownID, true)
	if err != nil {
		return nil, nil, err
	}
	if !liveIsRelease(projectID, applicationID, versionID, liveProjection, release) {
		return nil, nil, nil
	}
	if err := verifyReleasePins(ctx, tx, publicProjectID, projectID, release.ToolkitPins, nil); err != nil {
		var drift *agentrelease.DriftError
		if errors.As(err, &drift) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return &agentexecutionapp.ReleaseRun{ID: release.ID, ApplicationVersionID: release.ApplicationVersionID, DefinitionDigest: release.DefinitionDigest}, nil, nil
}

// withdrawnRefusal names the refusal of a withdrawn release whose missing
// toolkit is a sub-agent reference: the embedded copy the unpublish
// removed, not a drift the caller can repair by restoring a toolkit.
func withdrawnRefusal(release agentrelease.Release, err error) error {
	var drift *agentrelease.DriftError
	if release.State != agentrelease.StateWithdrawn || !errors.As(err, &drift) || drift.Reason != agentrelease.DriftToolkitMissing {
		return err
	}
	for _, pin := range release.ToolkitPins {
		if pin.ToolkitID == drift.ToolkitID && pin.Type == subAgentToolkitType {
			return fmt.Errorf("%w: toolkit %d", agentrelease.ErrWithdrawnDependenciesRemoved, pin.ToolkitID)
		}
	}
	return err
}

// subAgentToolkitType is the toolkit type of a sub-agent reference.
const subAgentToolkitType = "application"

// liveIsRelease reports whether the version row the run read is the
// release: the same row with the same definition digest.
func liveIsRelease(projectID, applicationID, versionID int64, liveProjection []byte, release agentrelease.Release) bool {
	if release.ApplicationVersionID != versionID {
		return false
	}
	live, err := agentidentity.VersionDefinitionDigest(projectID, applicationID, versionID, liveProjection)
	return err == nil && live == release.DefinitionDigest
}

// reconstructRelease is the release's definition with each toolkit's live
// settings put back, once every pin matches. The result is the projection
// text PostgreSQL gives for it, and its definition digest must be the
// release's: the run executes exactly the definition the release
// addresses, or it is refused.
func reconstructRelease(ctx context.Context, tx sqlExecutor, publicProjectID func() int64, release agentrelease.Release) ([]byte, error) {
	definition, err := agentrelease.DecodeObject(release.Definition)
	if err != nil {
		return nil, agentrelease.ErrDefinitionUnreadable
	}
	if agentrelease.ToolCount(definition) != len(release.ToolkitPins) {
		return nil, &agentrelease.DriftError{Reason: agentrelease.DriftToolkitSetChanged}
	}
	projected := map[int]any{}
	if err := verifyReleasePins(ctx, tx, publicProjectID, release.ProjectID, release.ToolkitPins, projected); err != nil {
		return nil, err
	}
	for _, pin := range release.ToolkitPins {
		if id, ok := agentrelease.ToolkitID(definition, pin.Index); !ok || id != pin.ToolkitID {
			return nil, &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftToolkitSetChanged}
		}
		if !agentrelease.WithToolSettings(definition, pin.Index, projected[pin.Index]) {
			return nil, &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftToolkitSetChanged}
		}
	}
	encoded, err := agentrelease.Encode(definition)
	if err != nil {
		return nil, err
	}
	var text string
	if err := tx.QueryRow(ctx, `SELECT $1::jsonb::text`, string(encoded)).Scan(&text); err != nil {
		return nil, fmt.Errorf("agent releases: normalise the release definition: %w", err)
	}
	digest, err := agentidentity.VersionDefinitionDigest(release.ProjectID, release.ApplicationID, release.ApplicationVersionID, []byte(text))
	if err != nil || digest != release.DefinitionDigest {
		return nil, &agentrelease.DriftError{Reason: agentrelease.DriftDefinitionMismatch}
	}
	return []byte(text), nil
}

// verifyReleasePins checks every pin against the live toolkit row and the
// configurations its references resolve to. projected, when not nil,
// receives each toolkit's projected settings by pin index. It runs inside
// the start transaction, so it reads in batches: one statement for every
// pinned toolkit row and at most two per schema for every configuration.
func verifyReleasePins(ctx context.Context, tx sqlExecutor, publicProjectID func() int64, projectID int64, pins []agentrelease.ToolkitPin, projected map[int]any) error {
	if len(pins) == 0 {
		return nil
	}
	schemaName, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(pins))
	var titles []string
	for _, pin := range pins {
		ids = append(ids, pin.ToolkitID)
		for _, configuration := range pin.Configurations {
			if !configuration.Private && !containsText(titles, configuration.Title) {
				titles = append(titles, configuration.Title)
			}
		}
	}
	settingsByID, err := readToolkitSettings(ctx, tx, schemaName, ids)
	if err != nil {
		return err
	}
	lookup := &AgentReleases{publicProjectID: publicProjectID}
	configurations, err := lookup.lookupConfigurations(ctx, tx, projectID, titles)
	if err != nil {
		return err
	}
	for _, pin := range pins {
		settings, ok := settingsByID[pin.ToolkitID]
		if !ok {
			return &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftToolkitMissing}
		}
		storedSHA, err := agentrelease.CanonicalSHA256([]byte(settings))
		if err != nil || storedSHA != pin.SettingsSHA256 {
			return &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftToolkitSettingsChanged}
		}
		if projected != nil {
			value, err := projectStoredSettings([]byte(settings), pin.SelectedTools)
			if err != nil {
				return &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftToolkitSettingsChanged}
			}
			projectedSHA, err := agentrelease.ValueSHA256(value)
			if err != nil || projectedSHA != pin.ProjectedSettingsSHA256 {
				return &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftToolkitSettingsChanged}
			}
			projected[pin.Index] = value
		}
		for _, configuration := range pin.Configurations {
			if configuration.Private {
				// Resolved in the runner's personal project at run time.
				continue
			}
			found := configurations[configuration.Title]
			switch {
			case configuration.ConfigurationUUID == "" && found.uuid == "":
			case configuration.ConfigurationUUID != "" && found.uuid == "":
				return &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftConfigurationMissing}
			case found.uuid != configuration.ConfigurationUUID || found.projectID != configuration.ProjectID ||
				found.dataSHA256 != configuration.DataSHA256:
				return &agentrelease.DriftError{ToolkitID: pin.ToolkitID, Reason: agentrelease.DriftConfigurationChanged}
			}
		}
	}
	return nil
}

// readToolkitSettings reads the stored settings of the toolkit rows ids
// name, in one statement. A row that is gone is absent from the answer.
func readToolkitSettings(ctx context.Context, tx sqlExecutor, schemaName string, ids []int64) (map[int64]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT id, COALESCE(settings::text, 'null') FROM %s.elitea_tools WHERE id = ANY($1::bigint[])`, schemaName), ids)
	if err != nil {
		return nil, fmt.Errorf("agent releases: read the pinned toolkits: %w", err)
	}
	defer rows.Close()
	settings := make(map[int64]string, len(ids))
	for rows.Next() {
		var (
			id    int64
			value string
		)
		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("agent releases: read a pinned toolkit: %w", err)
		}
		settings[id] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent releases: read the pinned toolkits: %w", err)
	}
	return settings, nil
}

// projectStoredSettings is the projected settings of a stored settings
// document: the selection applied to an object, any other value as is.
func projectStoredSettings(stored []byte, selected json.RawMessage) (any, error) {
	object, err := agentrelease.DecodeObject(stored)
	if err != nil {
		return decodeNumbersValue(stored)
	}
	return agentrelease.ProjectSettings(object, selected)
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func containsText(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}
