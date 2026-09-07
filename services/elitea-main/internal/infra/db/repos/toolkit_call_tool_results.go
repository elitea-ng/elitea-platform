package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ToolkitCallToolResultsRepository owns the output plane's half of one tool
// run: the admitted binding the terminal frame must match, and the durable
// insert of that frame.
//
// It writes ONLY `elitea_runtime.output_inbox`. There is no projection table
// and no replay event: a tool run has no transcript and no watching tab, and
// the producer's bounded wait reads the inbox row back directly.
type ToolkitCallToolResultsRepository struct {
	store sqlExecutor
	pool  *pgxpool.Pool
}

func NewToolkitCallToolResultsRepository(pool *pgxpool.Pool) (*ToolkitCallToolResultsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &ToolkitCallToolResultsRepository{store: store, pool: pool}, nil
}

func (r *ToolkitCallToolResultsRepository) ExpectedToolkitCallTool(
	ctx context.Context,
	executionID string,
	generation uint64,
) (outputapp.ExpectedToolkitCallTool, error) {
	if executionID == "" || generation == 0 || generation > math.MaxInt64 {
		return outputapp.ExpectedToolkitCallTool{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	queries := sqlcgen.New(r.pool)
	header, err := queries.GetExpectedToolkitCallToolHeader(ctx, sqlcgen.GetExpectedToolkitCallToolHeaderParams{
		ExecutionID: executionID,
		Generation:  int64(generation),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ExpectedToolkitCallTool{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	if err != nil {
		return outputapp.ExpectedToolkitCallTool{}, fmt.Errorf("load tool-run output binding: %w", err)
	}
	if header.Generation <= 0 || header.ResourceProjectID <= 0 || header.ProjectionProjectID <= 0 {
		return outputapp.ExpectedToolkitCallTool{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	bundleDigest, err := storedDigest(header.InputBundleDigest)
	if err != nil {
		return outputapp.ExpectedToolkitCallTool{}, fmt.Errorf("stored tool-run input bundle digest: %w", err)
	}
	entries, err := queries.GetToolkitCallToolInputEntries(ctx, sqlcgen.GetToolkitCallToolInputEntriesParams{
		ExecutionID: executionID,
		Generation:  int64(generation),
	})
	if err != nil {
		return outputapp.ExpectedToolkitCallTool{}, fmt.Errorf("load tool-run input entries: %w", err)
	}
	// Exactly two, one per role. A bundle that carried one entry under both
	// roles, or a third under neither, is the shape four other places already
	// refuse; refusing it here too keeps the output plane from being the one
	// seam that would accept it.
	if len(entries) != 2 {
		return outputapp.ExpectedToolkitCallTool{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	var settings, arguments outputapp.ToolkitCallToolInputBinding
	for _, entry := range entries {
		digest, err := storedDigest(entry.ContentDigest)
		if err != nil {
			return outputapp.ExpectedToolkitCallTool{}, fmt.Errorf("stored tool-run entry digest: %w", err)
		}
		binding := outputapp.ToolkitCallToolInputBinding{
			EntryID:          entry.EntryID,
			ImmutableVersion: entry.EntryVersion,
			ContentDigest:    digest,
		}
		switch entry.SemanticRole {
		case executiondomain.ToolkitCallToolSettingsRole:
			settings = binding
		case executiondomain.ToolkitCallToolArgumentsRole:
			arguments = binding
		default:
			return outputapp.ExpectedToolkitCallTool{}, outputapp.ErrInvalidToolkitCallToolOutput
		}
	}
	expected := outputapp.ExpectedToolkitCallTool{
		TenantID:            header.TenantID,
		ResourceProjectID:   strconv.FormatInt(int64(header.ResourceProjectID), 10),
		ProjectionProjectID: strconv.FormatInt(int64(header.ProjectionProjectID), 10),
		CapabilityID:        header.CapabilityID,
		CommandID:           header.CommandID,
		ExecutionID:         header.ExecutionID,
		Generation:          uint64(header.Generation),
		LogicalOutputID:     "toolkit-call-tool:" + header.ExecutionID,
		InputBundleID:       header.InputBundleID,
		InputBundleDigest:   bundleDigest,
		Settings:            settings,
		Arguments:           arguments,
	}
	if err := expected.Validate(); err != nil {
		return outputapp.ExpectedToolkitCallTool{}, fmt.Errorf("invalid stored tool-run output binding: %w", err)
	}
	return expected, nil
}

func (r *ToolkitCallToolResultsRepository) ProjectToolkitCallTool(
	ctx context.Context,
	projection outputapp.ToolkitCallToolProjection,
) (outputapp.ProjectionOutcome, error) {
	if err := projection.Frame.Validate(); err != nil ||
		projection.Frame.Sequence > math.MaxInt64 {
		if err == nil {
			err = outputapp.ErrInvalidToolkitCallToolOutput
		}
		return outputapp.ProjectionOutcome{}, err
	}
	record, err := toolkitCallToolOutputRecord(projection.Frame)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return outputapp.ProjectionOutcome{}, fmt.Errorf("begin tool-run projection: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	executor := pgxExecutor{queryer: tx}

	inserted := false
	existing, err := loadExistingOutput(ctx, executor, record)
	switch {
	case err == nil:
		if !sameDurableOutput(existing, record) {
			return outputapp.ProjectionOutcome{}, outputapp.ErrToolkitCallToolOutputConflict
		}
	case errors.Is(err, pgx.ErrNoRows):
		result, insertErr := insertOutputInbox(ctx, executor, record)
		if insertErr != nil {
			return outputapp.ProjectionOutcome{}, insertErr
		}
		if result.Inserted {
			inserted = true
			break
		}
		// Lost the race, or was refused. A row that is now there and identical
		// is a replay; anything else is a real refusal and must say which.
		existing, loadErr := loadExistingOutput(ctx, executor, record)
		if loadErr == nil {
			if !sameDurableOutput(existing, record) {
				return outputapp.ProjectionOutcome{}, outputapp.ErrToolkitCallToolOutputConflict
			}
			break
		}
		if !errors.Is(loadErr, pgx.ErrNoRows) {
			return outputapp.ProjectionOutcome{}, loadErr
		}
		if result.DeadlineRejected {
			return outputapp.ProjectionOutcome{}, outputapp.ErrOutputDeadlineExceeded
		}
		return outputapp.ProjectionOutcome{}, runtimedomain.ErrStaleFence
	default:
		return outputapp.ProjectionOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return outputapp.ProjectionOutcome{}, fmt.Errorf("commit tool-run projection: %w", err)
	}
	committed = true
	return outputapp.ProjectionOutcome{
		Inserted:          inserted,
		CommittedSequence: record.Sequence,
	}, nil
}

func toolkitCallToolOutputRecord(frame outputapp.ToolkitCallToolFrame) (outputRecord, error) {
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputRecord{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputRecord{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	return frameOutputRecord(
		frame.EventID, frame.LogicalOutputID, frame.StreamID, frame.TenantID,
		resourceProjectID, projectionProjectID, frame.Sequence,
		frame.ClaimHandoffWatermark, frame.OccurredAt, frame.Fence,
		payloadTypeToolkitCallToolResult, frame.PayloadDigest, frame.EncodedResult,
		frame.Settlement, frame.EncodedSettlement,
	), nil
}

var _ outputapp.ToolkitCallToolBindingRepository = (*ToolkitCallToolResultsRepository)(nil)
var _ outputapp.ToolkitCallToolProjector = (*ToolkitCallToolResultsRepository)(nil)
