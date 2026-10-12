package toolkits

// Deleting what an application engine holds for a toolkit (issue #1244).
//
// An Inventory toolkit's graph lives in the Inventory engine, not in the
// tenant's elitea_tools row, so `DELETE FROM elitea_tools` leaves it behind for
// ever. The delete is therefore ROW-FIRST WITH A JOURNAL, the shape of a
// project delete (#1245): ONE TRANSACTION deletes the toolkit row (RETURNING
// its type) and, when the type belongs to an application whose engine keeps
// data per toolkit, records the engine-side deletion owed in the cleanup
// journal (internal/application/toolkitcleanup, centry.toolkit_deletions). A
// reconciler drains the journal afterwards, with backoff.
//
// WHAT THE REQUEST DEPENDS ON: the database and nothing else. The toolkit is
// gone when the transaction commits; the engine is never called from the
// request, so an unreachable engine, a running ingest that holds the graph, or
// an engine that refuses this process does not keep a toolkit the user asked
// to delete. A database error deletes nothing and answers 500 as before. A row
// that does not exist answers as it always has (204: the route's contract
// documents no 404), and journals nothing.
//
// WHICH TYPES OWE A JOURNAL ROW is the journal's decision
// (toolkitcleanup.Journal.AppToEnqueue): Inventory types on a replica with the
// Inventory client; DeepWiki types never (DeepWiki keeps nothing per toolkit);
// every other type never.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// ToolkitCleanupJournal records, inside the delete's transaction, the
// engine-side deletion a toolkit owes. *toolkitcleanup.Journal satisfies it.
type ToolkitCleanupJournal interface {
	// Enqueue writes the journal row for the toolkit when its type owes one on
	// this replica, and returns the application queued ("" when none).
	Enqueue(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64, toolkitType string) (app string, err error)
}

// WithToolkitCleanupJournal makes Delete row-first with a journal
// (Repository.DeleteToolkitRowFirst). Without it Delete removes the row alone,
// which is what every deployment did before the engines served a platform
// service.
func WithToolkitCleanupJournal(journal ToolkitCleanupJournal) Option {
	return func(h *Handler) {
		if journal == nil {
			return
		}
		h.toolkitCleanup = journal
	}
}

// DeleteToolkitRowFirst implements Repository: one transaction that deletes the
// toolkit row and lets journal record the engine-side cleanup its type owes. A
// row that does not exist is not an error and journals nothing; any database
// error (the delete's or the journal's) rolls both back.
func (r *pgRepo) DeleteToolkitRowFirst(ctx context.Context, projectID, toolkitID string, journal ToolkitCleanupJournal) error {
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var toolkitType string
	switch err := tx.QueryRow(ctx,
		fmt.Sprintf(`DELETE FROM %s.elitea_tools WHERE id = $1 RETURNING type`, schema), toolkitID,
	).Scan(&toolkitType); {
	case errors.Is(err, pgx.ErrNoRows):
		// Nothing to delete and nothing to journal; the empty transaction ends
		// with the rollback above.
		return nil
	case err != nil:
		return err
	}
	if journal != nil {
		project, projectErr := strconv.ParseInt(projectID, 10, 64)
		toolkit, toolkitErr := strconv.ParseInt(toolkitID, 10, 64)
		if projectErr != nil || toolkitErr != nil {
			// The tenant schema name and the DELETE already accepted both as
			// integers; this cannot happen, and a journal keyed by them must
			// not be guessed.
			return fmt.Errorf("toolkit %q of project %q: ids are not integers", toolkitID, projectID)
		}
		if _, err := journal.Enqueue(ctx, tx, project, toolkit, toolkitType); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
