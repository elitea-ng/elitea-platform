package repos

import (
	"context"
	"errors"
	"fmt"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func (r *CurrentConfigurationsRepository) ListCurrentConfigurationOptionCandidates(
	ctx context.Context,
	query configurationapp.CurrentConfigurationOptionCandidatesQuery,
) ([]configurationapp.CurrentConfigurationOption, error) {
	if err := validateCurrentConfigurationOptionCandidatesQuery(ctx, query); err != nil {
		return nil, err
	}

	items := make([]configurationapp.CurrentConfigurationOption, 0)
	projectItems, err := r.listCurrentConfigurationOptionScope(
		ctx,
		query.ProjectID,
		false,
		query.Types,
		query.Sections,
		query.MaxRows,
	)
	if err != nil {
		return nil, err
	}
	items = append(items, projectItems...)

	if !query.IncludeShared ||
		query.ProjectID == query.PublicProjectID ||
		len(items) == query.MaxRows {
		return items, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	publicItems, err := r.listCurrentConfigurationOptionScope(
		ctx,
		query.PublicProjectID,
		true,
		query.Types,
		query.Sections,
		query.MaxRows-len(items),
	)
	if err != nil {
		return nil, err
	}
	items = append(items, publicItems...)
	return items, nil
}

func (r *CurrentConfigurationsRepository) listCurrentConfigurationOptionScope(
	ctx context.Context,
	projectID int32,
	sharedOnly bool,
	types []string,
	sections []string,
	maxRows int,
) ([]configurationapp.CurrentConfigurationOption, error) {
	items := make([]configurationapp.CurrentConfigurationOption, 0)
	err := r.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		queries, err := r.queries(tx)
		if err != nil {
			return err
		}
		rows, err := queries.ListCurrentConfigurationOptionCandidates(
			ctx,
			sqlcgen.ListCurrentConfigurationOptionCandidatesParams{
				ProjectID:  projectID,
				Types:      append([]string(nil), types...),
				Sections:   append([]string(nil), sections...),
				SharedOnly: sharedOnly,
				LimitRows:  int32(maxRows),
			},
		)
		if err != nil {
			return fmt.Errorf("list current configuration option candidates: %w", err)
		}
		if len(rows) > maxRows {
			return errors.New("current configuration option candidate query exceeded its row bound")
		}

		items = make([]configurationapp.CurrentConfigurationOption, 0, len(rows))
		for _, row := range rows {
			if row.ProjectID != projectID || (sharedOnly && !row.Shared) {
				return errors.New("current configuration option candidate escaped its authorized scope")
			}
			items = append(items, configurationapp.CurrentConfigurationOption{
				EliteaTitle: row.EliteaTitle,
				Label:       row.Label,
				Type:        row.Type,
				Section:     row.Section,
				Shared:      row.Shared,
				ProjectID:   row.ProjectID,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func validateCurrentConfigurationOptionCandidatesQuery(
	ctx context.Context,
	query configurationapp.CurrentConfigurationOptionCandidatesQuery,
) error {
	if ctx == nil ||
		query.ProjectID <= 0 ||
		query.PublicProjectID <= 0 ||
		query.MaxRows <= 0 ||
		query.MaxRows > configurationapp.MaxCurrentConfigurationOptionCandidates+1 ||
		len(query.Types)+len(query.Sections) == 0 ||
		len(query.Types) > configurationapp.MaxCurrentConfigurationOptionCandidates ||
		len(query.Sections) > configurationapp.MaxCurrentConfigurationOptionCandidates {
		return configurationapp.ErrInvalidCurrentConfigurationOptionsRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, values := range [][]string{query.Types, query.Sections} {
		for _, value := range values {
			if value == "" || len(value) > configurationapp.MaxCurrentConfigurationTypeLength {
				return configurationapp.ErrInvalidCurrentConfigurationOptionsRequest
			}
		}
	}
	return nil
}

var _ configurationapp.CurrentConfigurationOptionCandidates = (*CurrentConfigurationsRepository)(nil)
