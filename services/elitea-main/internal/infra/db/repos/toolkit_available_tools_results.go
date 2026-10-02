package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ToolkitAvailableToolsResultsRepository struct {
	store sqlExecutor
	pool  *pgxpool.Pool
}

func NewToolkitAvailableToolsResultsRepository(pool *pgxpool.Pool) (*ToolkitAvailableToolsResultsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &ToolkitAvailableToolsResultsRepository{store: store, pool: pool}, nil
}

func (r *ToolkitAvailableToolsResultsRepository) ExpectedToolkitAvailableTools(
	ctx context.Context,
	executionID string,
	generation uint64,
) (outputapp.ExpectedToolkitAvailableTools, error) {
	if executionID == "" || generation == 0 || generation > math.MaxInt64 {
		return outputapp.ExpectedToolkitAvailableTools{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	queries := sqlcgen.New(r.pool)
	header, err := queries.GetExpectedToolkitAvailableToolsHeader(ctx, sqlcgen.GetExpectedToolkitAvailableToolsHeaderParams{
		ExecutionID: executionID,
		Generation:  int64(generation),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ExpectedToolkitAvailableTools{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	if err != nil {
		return outputapp.ExpectedToolkitAvailableTools{}, fmt.Errorf("load discovery output binding: %w", err)
	}
	if header.Generation <= 0 || header.ResourceProjectID <= 0 || header.ProjectionProjectID <= 0 {
		return outputapp.ExpectedToolkitAvailableTools{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	bundleDigest, err := storedDigest(header.InputBundleDigest)
	if err != nil {
		return outputapp.ExpectedToolkitAvailableTools{}, fmt.Errorf("stored discovery input bundle digest: %w", err)
	}
	entries, err := queries.GetToolkitAvailableToolsInputEntries(ctx, sqlcgen.GetToolkitAvailableToolsInputEntriesParams{
		ExecutionID: executionID,
		Generation:  int64(generation),
	})
	if err != nil {
		return outputapp.ExpectedToolkitAvailableTools{}, fmt.Errorf("load discovery input entries: %w", err)
	}
	if len(entries) != 2 {
		return outputapp.ExpectedToolkitAvailableTools{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	var settings outputapp.ToolkitAvailableToolsInputBinding
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.SemanticRole] {
			return outputapp.ExpectedToolkitAvailableTools{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
		}
		seen[entry.SemanticRole] = true
		digest, err := storedDigest(entry.ContentDigest)
		if err != nil {
			return outputapp.ExpectedToolkitAvailableTools{}, fmt.Errorf("stored discovery entry digest: %w", err)
		}
		binding := outputapp.ToolkitAvailableToolsInputBinding{
			EntryID:          entry.EntryID,
			ImmutableVersion: entry.EntryVersion,
			ContentDigest:    digest,
		}
		switch entry.SemanticRole {
		case executiondomain.ToolkitAvailableToolsSettingsRole:
			settings = binding
		case "toolkit.available_tools.runtime_context":
			if err := binding.Validate(); err != nil {
				return outputapp.ExpectedToolkitAvailableTools{}, err
			}
		default:
			return outputapp.ExpectedToolkitAvailableTools{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
		}
	}
	toolkitType, err := toolkitAvailableToolsPreparedType(header, settings)
	if err != nil {
		return outputapp.ExpectedToolkitAvailableTools{}, err
	}
	expected := outputapp.ExpectedToolkitAvailableTools{
		TenantID:            header.TenantID,
		ResourceProjectID:   strconv.FormatInt(int64(header.ResourceProjectID), 10),
		ProjectionProjectID: strconv.FormatInt(int64(header.ProjectionProjectID), 10),
		CapabilityID:        header.CapabilityID,
		CommandID:           header.CommandID,
		ExecutionID:         header.ExecutionID,
		Generation:          uint64(header.Generation),
		LogicalOutputID:     "toolkit-available-tools:" + header.ExecutionID,
		InputBundleID:       header.InputBundleID,
		InputBundleDigest:   bundleDigest,
		Settings:            settings,
		ToolkitType:         toolkitType,
	}
	if err := expected.Validate(); err != nil {
		return outputapp.ExpectedToolkitAvailableTools{}, fmt.Errorf("invalid stored discovery output binding: %w", err)
	}
	return expected, nil
}

func (r *ToolkitAvailableToolsResultsRepository) ProjectToolkitAvailableTools(
	ctx context.Context,
	projection outputapp.ToolkitAvailableToolsProjection,
) (outputapp.ProjectionOutcome, error) {
	if err := projection.Frame.Validate(); err != nil ||
		projection.Frame.Sequence > math.MaxInt64 {
		if err == nil {
			err = outputapp.ErrInvalidToolkitAvailableToolsOutput
		}
		return outputapp.ProjectionOutcome{}, err
	}
	record, err := toolkitAvailableToolsOutputRecord(projection.Frame)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return outputapp.ProjectionOutcome{}, fmt.Errorf("begin discovery projection: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	executor := pgxExecutor{queryer: tx}
	if err := verifyToolkitAvailableToolsArtifact(ctx, sqlcgen.New(tx), projection.Frame); err != nil {
		return outputapp.ProjectionOutcome{}, err
	}

	inserted := false
	existing, err := loadExistingOutput(ctx, executor, record)
	switch {
	case err == nil:
		if !sameDurableOutput(existing, record) {
			return outputapp.ProjectionOutcome{}, outputapp.ErrToolkitAvailableToolsOutputConflict
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
		existing, loadErr := loadExistingOutput(ctx, executor, record)
		if loadErr == nil {
			if !sameDurableOutput(existing, record) {
				return outputapp.ProjectionOutcome{}, outputapp.ErrToolkitAvailableToolsOutputConflict
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
	if err := markOutputProjected(ctx, executor, record.EventID); err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return outputapp.ProjectionOutcome{}, fmt.Errorf("commit discovery projection: %w", err)
	}
	committed = true
	return outputapp.ProjectionOutcome{
		Inserted:          inserted,
		CommittedSequence: record.Sequence,
	}, nil
}

func toolkitAvailableToolsOutputRecord(frame outputapp.ToolkitAvailableToolsFrame) (outputRecord, error) {
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputRecord{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputRecord{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	return frameOutputRecord(
		frame.EventID, frame.LogicalOutputID, frame.StreamID, frame.TenantID,
		resourceProjectID, projectionProjectID, frame.Sequence,
		frame.ClaimHandoffWatermark, frame.OccurredAt, frame.Fence,
		payloadTypeToolkitAvailableToolsResult, frame.PayloadDigest, frame.EncodedResult,
		frame.Settlement, frame.EncodedSettlement,
	), nil
}

var _ outputapp.ToolkitAvailableToolsBindingRepository = (*ToolkitAvailableToolsResultsRepository)(nil)
var _ outputapp.ToolkitAvailableToolsProjector = (*ToolkitAvailableToolsResultsRepository)(nil)

func verifyToolkitAvailableToolsArtifact(ctx context.Context, queries *sqlcgen.Queries, frame outputapp.ToolkitAvailableToolsFrame) error {
	stored, err := queries.GetToolkitAvailableToolsArtifactForOutput(ctx, sqlcgen.GetToolkitAvailableToolsArtifactForOutputParams{
		ExecutionID: frame.Fence.ExecutionID, Generation: int64(frame.Fence.Generation),
		ArtifactID: frame.Result.ResultArtifact.ArtifactID, ImmutableVersion: frame.Result.ResultArtifact.ImmutableVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ErrToolkitAvailableToolsBindingMismatch
	}
	if err != nil {
		return fmt.Errorf("load discovery result artifact: %w", err)
	}
	digest, err := storedDigest(stored.Digest)
	if err != nil {
		return outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	settingsDigest, err := storedDigest(stored.SettingsContentDigest)
	if err != nil {
		return outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	result := frame.Result
	artifact := result.ResultArtifact
	if stored.ArtifactID != artifact.ArtifactID || stored.ImmutableVersion != artifact.ImmutableVersion ||
		stored.ByteLength <= 0 || uint64(stored.ByteLength) != artifact.ByteLength || digest != artifact.Digest ||
		stored.MediaType != artifact.MediaType || stored.Classification != artifact.Classification || !stored.BytesVerifiedAt.Valid || stored.BytesVerifiedAt.Time.IsZero() ||
		stored.InputBundleID != result.InputBundleID || stored.SettingsEntryID != result.Settings.EntryID ||
		stored.SettingsEntryVersion != result.Settings.ImmutableVersion || settingsDigest != result.Settings.ContentDigest {
		return outputapp.ErrToolkitAvailableToolsBindingMismatch
	}
	return nil
}

// Read the exact Main-owned envelope selected before publication.
func toolkitAvailableToolsPreparedType(header sqlcgen.GetExpectedToolkitAvailableToolsHeaderRow, settings outputapp.ToolkitAvailableToolsInputBinding) (string, error) {
	toolkitType, err := discovery.PreparedToolkitType(header.PreparedSignedEnvelopeBytes, header.PreparedSignedEnvelopeDigest, discovery.PreparedIdentity{
		CommandID: header.CommandID, ExecutionID: header.ExecutionID, Generation: uint64(header.Generation), TenantID: header.TenantID,
		ResourceProjectID: strconv.FormatInt(int64(header.ResourceProjectID), 10), ProjectionProjectID: strconv.FormatInt(int64(header.ProjectionProjectID), 10),
		InputBundleID: header.InputBundleID, InputBundleDigest: header.InputBundleDigest, SettingsEntryID: settings.EntryID,
	})
	if err != nil {
		return "", outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	return toolkitType, nil
}
