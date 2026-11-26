package main

// `elitea-main break-glass note` and `list --elevations` (ADR-0059 decision
// 5, "Review after use", "Notes"; story R2.9c, issue 2604). See
// break_glass.go.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// breakGlassElevationsLimit is how many elevations `list --elevations`
// prints, newest first.
const breakGlassElevationsLimit = 200

// breakGlassNoteTx attaches the operator's note.
func breakGlassNoteTx(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, req breakGlassRequest, at time.Time) ([]string, error) {
	id := req.ids[0]
	result, err := adminaccess.AddElevationNote(ctx, tx, appender, adminaccess.ElevationNote{
		ID: id, Text: req.note, Source: adminaccess.NoteSourceOperator, At: at,
	})
	switch {
	case errors.Is(err, adminaccess.ErrElevationNotFound):
		return nil, refusedError("no elevation %s", id)
	case errors.Is(err, adminaccess.ErrElevationInvalid):
		return nil, refusedError("%v", err)
	case err != nil:
		return nil, err
	}
	return []string{fmt.Sprintf("added note %s to elevation %s; its state is still %s",
		result.NoteID, id, result.Elevation.State(at))}, nil
}

// breakGlassListElevationsTx prints the newest elevations: id, person, level,
// path and state; never the reason text.
func breakGlassListElevationsTx(ctx context.Context, tx pgx.Tx, at time.Time) ([]string, error) {
	elevations, err := adminaccess.RecentElevations(ctx, tx, breakGlassElevationsLimit, at)
	if err != nil {
		return nil, err
	}
	if len(elevations) == 0 {
		return []string{"no elevation"}, nil
	}
	lines := make([]string, 0, len(elevations))
	for _, e := range elevations {
		lines = append(lines, fmt.Sprintf("%s %s user=%d level=%s path=%s requested=%s",
			e.ID, e.State, e.UserID, e.Level, e.Path, e.RequestedAt.UTC().Format(time.RFC3339)))
	}
	return lines, nil
}
