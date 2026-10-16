package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentIndexMetaScheduleRepository owns only the second current DELETE
// transaction in p_<project_id>. PgVector deletion is deliberately outside
// this repository and commits before this method is called.
type CurrentIndexMetaScheduleRepository struct {
	projects projectStore
}

func NewCurrentIndexMetaScheduleRepository(
	pool *pgxpool.Pool,
) (*CurrentIndexMetaScheduleRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentIndexMetaScheduleRepository(projects)
}

func newCurrentIndexMetaScheduleRepository(
	projects projectStore,
) (*CurrentIndexMetaScheduleRepository, error) {
	if projects == nil {
		return nil, errors.New("current index metadata schedule database is required")
	}
	return &CurrentIndexMetaScheduleRepository{projects: projects}, nil
}

func (r *CurrentIndexMetaScheduleRepository) DeleteCurrentIndexSchedule(
	ctx context.Context,
	projectID, toolkitID int32,
	indexName string,
) error {
	if r == nil || r.projects == nil || ctx == nil ||
		projectID <= 0 || toolkitID <= 0 ||
		!validCurrentIndexMetaScheduleName(indexName) {
		return indexmetaapp.ErrCurrentIndexScheduleUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	err := r.projects.WithinProjectTx(
		ctx,
		int64(projectID),
		pgx.TxOptions{
			IsoLevel:   pgx.ReadCommitted,
			AccessMode: pgx.ReadWrite,
		},
		func(tx sqlExecutor) error {
			var rawMeta []byte
			err := tx.QueryRow(
				ctx,
				`SELECT meta FROM elitea_tools WHERE id = $1 FOR UPDATE`,
				toolkitID,
			).Scan(&rawMeta)
			if errors.Is(err, pgx.ErrNoRows) {
				return indexmetaapp.ErrCurrentIndexScheduleToolkitMissing
			}
			if err != nil {
				return fmt.Errorf("load current index metadata schedule: %w", err)
			}
			present, err := currentIndexSchedulePresent(rawMeta, indexName)
			if err != nil {
				return err
			}
			if !present {
				return nil
			}

			tag, err := tx.Exec(ctx, `
UPDATE elitea_tools
SET meta = jsonb_set(
        meta,
        '{indexes_meta}',
        (meta->'indexes_meta') - $2::text,
        false
    ),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`,
				toolkitID,
				indexName,
			)
			if err != nil {
				return fmt.Errorf("delete current index metadata schedule: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return indexmetaapp.ErrCurrentIndexScheduleUnavailable
			}
			return nil
		},
	)
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, indexmetaapp.ErrCurrentIndexScheduleToolkitMissing) {
		return err
	}
	return indexmetaapp.ErrCurrentIndexScheduleUnavailable
}

func currentIndexSchedulePresent(raw []byte, indexName string) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var metadata map[string]any
	if err := decoder.Decode(&metadata); err != nil {
		return false, indexmetaapp.ErrCurrentIndexScheduleUnavailable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false, indexmetaapp.ErrCurrentIndexScheduleUnavailable
	}
	if metadata == nil {
		return false, nil
	}
	rawIndexes, present := metadata["indexes_meta"]
	if !present {
		return false, nil
	}
	if rawIndexes == nil {
		return false, indexmetaapp.ErrCurrentIndexScheduleUnavailable
	}
	indexes, ok := rawIndexes.(map[string]any)
	if !ok {
		return false, indexmetaapp.ErrCurrentIndexScheduleUnavailable
	}
	_, present = indexes[indexName]
	return present, nil
}

func validCurrentIndexMetaScheduleName(value string) bool {
	return value != "" &&
		len(value) <= indexmetaapp.MaxCurrentIndexMetaCollectionBytes &&
		utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

var _ indexmetaapp.ScheduleCleaner = (*CurrentIndexMetaScheduleRepository)(nil)
