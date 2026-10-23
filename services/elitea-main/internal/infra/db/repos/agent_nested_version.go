package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentNestedApplicationVersionRepository reads one saved application version
// out of a project's tenant schema for the private nested-agent route.
//
// It deliberately does NOT reuse CurrentAgentStartRepository: that type resolves
// a conversation TURN, and every one of its queries is anchored on a
// conversation, an author participant and a question. A nested child has none of
// those — it is a tool inside the parent's turn — so sharing the type would only
// share a name.
type CurrentNestedApplicationVersionRepository struct {
	projects projectStore
}

func NewCurrentNestedApplicationVersionRepository(
	pool *pgxpool.Pool,
) (*CurrentNestedApplicationVersionRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentNestedApplicationVersionRepository(projects)
}

func newCurrentNestedApplicationVersionRepository(
	projects projectStore,
) (*CurrentNestedApplicationVersionRepository, error) {
	if projects == nil {
		return nil, errors.New("current nested application project database is required")
	}
	return &CurrentNestedApplicationVersionRepository{projects: projects}, nil
}

type currentApplicationVersionQuerier interface {
	ResolveCurrentApplicationVersionDetails(
		context.Context,
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams,
	) (sqlcgen.ResolveCurrentApplicationVersionDetailsRow, error)
}

// ReadCurrentApplicationVersion opens the tenant schema of the project the
// CLAIM resolved and returns that project's copy of the requested version.
//
// The project is a parameter and never a column of the row: `application_versions`
// lives inside the per-project schema, so scoping is the schema selection itself.
// A caller that passed a project the claim did not authorize would read the wrong
// tenant — which is why the only caller
// (storage.RuntimeApplicationVersionService.Resolve) derives it from
// AuthorizeRuntimeContext and nothing else.
func (repository *CurrentNestedApplicationVersionRepository) ReadCurrentApplicationVersion(
	ctx context.Context,
	projectID int64,
	applicationID int64,
	versionID int64,
) (storage.CurrentApplicationVersionRecord, error) {
	if repository == nil || repository.projects == nil || ctx == nil {
		return storage.CurrentApplicationVersionRecord{}, errors.New(
			"current nested application version repository is unavailable",
		)
	}
	if projectID <= 0 {
		return storage.CurrentApplicationVersionRecord{}, errors.New(
			"current nested application project is required",
		)
	}
	applicationKey, applicationKeyValid := currentAgentDatabaseID(applicationID)
	versionKey, versionKeyValid := currentAgentDatabaseID(versionID)
	if !applicationKeyValid || !versionKeyValid {
		return storage.CurrentApplicationVersionRecord{}, storage.ErrContentNotFound
	}
	var record storage.CurrentApplicationVersionRecord
	err := repository.projects.WithinProjectTx(
		ctx,
		projectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentApplicationVersionQuerier)
			if !ok {
				return errors.New("current nested application version query is unavailable")
			}
			row, queryErr := queries.ResolveCurrentApplicationVersionDetails(
				ctx,
				sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
					ApplicationVersionID: versionKey,
					ApplicationID:        applicationKey,
				},
			)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return storage.ErrContentNotFound
			}
			if queryErr != nil {
				return fmt.Errorf("resolve current application version details: %w", queryErr)
			}
			versionDetails := json.RawMessage(row.ApplicationVersionDetailsJson)
			if row.ApplicationID != applicationKey || row.ApplicationVersionID != versionKey ||
				!json.Valid(versionDetails) {
				return storage.ErrContentNotFound
			}
			record = storage.CurrentApplicationVersionRecord{
				ApplicationID:  int64(row.ApplicationID),
				VersionID:      int64(row.ApplicationVersionID),
				VersionDetails: versionDetails,
			}
			return nil
		},
	)
	if err != nil {
		return storage.CurrentApplicationVersionRecord{}, err
	}
	return record, nil
}

var _ storage.CurrentApplicationVersionSource = (*CurrentNestedApplicationVersionRepository)(nil)

// NestedApplicationTools is the frozen `tools` of one saved sub-agent version
// of projectID (agentexecution.NestedApplicationTools): the version-details
// projection ReadCurrentApplicationVersion reads, which the nested-agent
// runtime route freezes the child from. An absent version is an error (the
// admission is refused rather than stored with a guessed set).
func (repository *CurrentNestedApplicationVersionRepository) NestedApplicationTools(
	ctx context.Context,
	projectID, applicationID, versionID int64,
) (json.RawMessage, error) {
	record, err := repository.ReadCurrentApplicationVersion(ctx, projectID, applicationID, versionID)
	if err != nil {
		return nil, err
	}
	var details struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(record.VersionDetails, &details); err != nil {
		return nil, fmt.Errorf("decode the sub-agent's version details: %w", err)
	}
	return details.Tools, nil
}
