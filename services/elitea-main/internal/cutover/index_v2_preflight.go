package cutover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxIndexV1SpoolRoots = 256

var ErrIndexV2CutoverBlocked = errors.New("index capability v2 cutover is blocked")

// IndexV1PersistedState is the exact durable version-1 state that must be
// terminally reconciled before Main and workers switch to index capability
// version 2.
type IndexV1PersistedState struct {
	LiveJobs                   int64 `json:"live_jobs"`
	OutstandingOutbox          int64 `json:"outstanding_outbox"`
	ActiveClaims               int64 `json:"active_claims"`
	PendingInitializations     int64 `json:"pending_initializations"`
	PendingTerminalProjections int64 `json:"pending_terminal_projections"`
	PendingManualCleanups      int64 `json:"pending_manual_cleanups"`
}

// IndexControlState covers the complete version-1 source stream/group. Every
// reference, pending entry and delivery mapping must be absent before the
// coordinated switch to a new version-2 stream/group.
type IndexControlState struct {
	StreamEntries    int64 `json:"stream_entries"`
	PendingEntries   int64 `json:"pending_entries"`
	DeliveryMappings int64 `json:"delivery_mappings"`
}

type IndexV1PersistedStateReader interface {
	ReadIndexV1CutoverState(context.Context) (IndexV1PersistedState, error)
}

type IndexControlStateReader interface {
	ReadIndexControlState(context.Context) (IndexControlState, error)
}

type IndexV2PreflightReport struct {
	Persisted        IndexV1PersistedState `json:"persisted"`
	Control          IndexControlState     `json:"control"`
	SpoolRoots       int                   `json:"spool_roots"`
	NonEmptySpoolDir int                   `json:"non_empty_spool_roots"`
}

type IndexV2Preflight struct {
	persisted IndexV1PersistedStateReader
	control   IndexControlStateReader
	roots     []string
}

func NewIndexV2Preflight(
	persisted IndexV1PersistedStateReader,
	control IndexControlStateReader,
	spoolRoots []string,
) (*IndexV2Preflight, error) {
	if persisted == nil || control == nil {
		return nil, errors.New("index v2 preflight state readers are required")
	}
	roots, err := validateIndexV1SpoolRoots(spoolRoots)
	if err != nil {
		return nil, err
	}
	return &IndexV2Preflight{
		persisted: persisted,
		control:   control,
		roots:     roots,
	}, nil
}

func (p *IndexV2Preflight) Check(ctx context.Context) (IndexV2PreflightReport, error) {
	if ctx == nil {
		return IndexV2PreflightReport{}, errors.New("index v2 preflight context is required")
	}
	if err := ctx.Err(); err != nil {
		return IndexV2PreflightReport{}, err
	}
	persisted, err := p.persisted.ReadIndexV1CutoverState(ctx)
	if err != nil {
		return IndexV2PreflightReport{}, fmt.Errorf("read persisted index v1 state: %w", err)
	}
	if err := validateIndexV1PersistedState(persisted); err != nil {
		return IndexV2PreflightReport{}, err
	}
	control, err := p.control.ReadIndexControlState(ctx)
	if err != nil {
		return IndexV2PreflightReport{}, fmt.Errorf("read index control state: %w", err)
	}
	if err := validateIndexControlState(control); err != nil {
		return IndexV2PreflightReport{}, err
	}
	nonEmpty, err := countNonEmptyIndexSpoolRoots(p.roots)
	if err != nil {
		return IndexV2PreflightReport{}, err
	}
	report := IndexV2PreflightReport{
		Persisted:        persisted,
		Control:          control,
		SpoolRoots:       len(p.roots),
		NonEmptySpoolDir: nonEmpty,
	}
	if persisted.LiveJobs != 0 ||
		persisted.OutstandingOutbox != 0 ||
		persisted.ActiveClaims != 0 ||
		persisted.PendingInitializations != 0 ||
		persisted.PendingTerminalProjections != 0 ||
		persisted.PendingManualCleanups != 0 ||
		control.StreamEntries != 0 ||
		control.PendingEntries != 0 ||
		control.DeliveryMappings != 0 ||
		nonEmpty != 0 {
		return report, fmt.Errorf(
			"%w: live_jobs=%d outstanding_outbox=%d active_claims=%d pending_initializations=%d pending_terminal_projections=%d pending_manual_cleanups=%d stream_entries=%d pending_entries=%d delivery_mappings=%d non_empty_spool_roots=%d",
			ErrIndexV2CutoverBlocked,
			persisted.LiveJobs,
			persisted.OutstandingOutbox,
			persisted.ActiveClaims,
			persisted.PendingInitializations,
			persisted.PendingTerminalProjections,
			persisted.PendingManualCleanups,
			control.StreamEntries,
			control.PendingEntries,
			control.DeliveryMappings,
			nonEmpty,
		)
	}
	return report, nil
}

func validateIndexV1PersistedState(state IndexV1PersistedState) error {
	if state.LiveJobs < 0 ||
		state.OutstandingOutbox < 0 ||
		state.ActiveClaims < 0 ||
		state.PendingInitializations < 0 ||
		state.PendingTerminalProjections < 0 ||
		state.PendingManualCleanups < 0 {
		return errors.New("persisted index v1 state contains a negative count")
	}
	return nil
}

func validateIndexControlState(state IndexControlState) error {
	if state.StreamEntries < 0 || state.PendingEntries < 0 || state.DeliveryMappings < 0 {
		return errors.New("index control state contains a negative count")
	}
	return nil
}

func validateIndexV1SpoolRoots(roots []string) ([]string, error) {
	if len(roots) == 0 || len(roots) > maxIndexV1SpoolRoots {
		return nil, fmt.Errorf("index v1 spool roots must contain 1 through %d paths", maxIndexV1SpoolRoots)
	}
	seen := make(map[string]struct{}, len(roots))
	validated := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, errors.New("index v1 spool roots must be absolute canonical paths")
		}
		if _, duplicate := seen[root]; duplicate {
			return nil, errors.New("index v1 spool roots must be unique")
		}
		seen[root] = struct{}{}
		validated = append(validated, root)
	}
	return validated, nil
}

func countNonEmptyIndexSpoolRoots(roots []string) (int, error) {
	nonEmpty := 0
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil {
			return 0, fmt.Errorf("inspect index v1 spool root: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return 0, errors.New("index v1 spool root must be a directory, not a symlink")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return 0, errors.New("index v1 spool root must not grant group or other permissions")
		}
		directory, err := os.Open(root)
		if err != nil {
			return 0, fmt.Errorf("open index v1 spool root: %w", err)
		}
		_, readErr := directory.ReadDir(1)
		closeErr := directory.Close()
		switch {
		case readErr == nil:
			nonEmpty++
		case errors.Is(readErr, io.EOF):
		default:
			return 0, fmt.Errorf("read index v1 spool root: %w", readErr)
		}
		if closeErr != nil {
			return 0, fmt.Errorf("close index v1 spool root: %w", closeErr)
		}
	}
	return nonEmpty, nil
}
