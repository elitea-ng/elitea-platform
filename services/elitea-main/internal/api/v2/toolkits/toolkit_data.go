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
// (toolkitcleanup.AppToEnqueue), from the type alone: the Inventory types (an
// exact allow-list), on every replica whether or not it has the Inventory
// client — a replica that has one drains the row; DeepWiki types never
// (DeepWiki keeps nothing per toolkit); every other type never.
//
// THE TOOLKIT'S INDEXES GO THE SAME WAY (ADR-0031 decision 4). Under the Rust
// indexing runtime an indexing toolkit's indexes are rows of
// elitea_runtime.index_registry (shared schema, keyed by project and toolkit
// id, no foreign key to the tenant row) and their vectors are namespaces in
// elitea-vector. Deleting the toolkit row alone would orphan both for ever.
// So the same transaction TOMBSTONES every live registry row of the toolkit
// (deleted_at, exactly what deleting one index does), and the registry's
// tombstone sweeper deletes each namespace and purges the row afterwards,
// with its backoff; a row whose run is still active waits until the run ends
// (the sweeper's claim skips it). Under the Python runtime the toolkit has no
// registry rows and the statement touches nothing; on a database that has not
// applied the shared migrations the table does not exist and the statement is
// skipped (tombstoneToolkitIndexes).

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
	// Enqueue writes the journal row for the toolkit when its type owes one,
	// and returns the application queued ("" when none).
	Enqueue(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64, toolkitType string) (app string, err error)
}

// MemoryCleanupJournal is the part of the journal a MEMORY toolkit's delete
// uses: the vector store holds its memories stamped with the toolkit id, so the
// delete records that id. *toolkitcleanup.Journal satisfies it; a journal that
// does not is simply not asked.
type MemoryCleanupJournal interface {
	EnqueueMemory(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64) (app string, err error)
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
	project, projectErr := strconv.ParseInt(projectID, 10, 64)
	toolkit, toolkitErr := strconv.ParseInt(toolkitID, 10, 64)
	if projectErr != nil || toolkitErr != nil {
		// The tenant schema name and the DELETE already accepted both as
		// integers; this cannot happen, and a journal or a registry keyed by
		// them must not be guessed.
		return fmt.Errorf("toolkit %q of project %q: ids are not integers", toolkitID, projectID)
	}
	if err := tombstoneToolkitIndexes(ctx, tx, project, toolkit); err != nil {
		return err
	}
	if journal != nil {
		if _, err := journal.Enqueue(ctx, tx, project, toolkit, toolkitType); err != nil {
			return err
		}
		// A memory toolkit's memories are in the vector store, stamped with
		// the toolkit id (every namespace of it, every owner's): the delete
		// records them, so a rename or a changed namespace setting orphans
		// nothing.
		if memory, ok := journal.(MemoryCleanupJournal); ok && toolkitType == "memory" {
			if _, err := memory.EnqueueMemory(ctx, tx, project, toolkit); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// tombstoneToolkitIndexes marks every live index_registry row of the toolkit
// deleted, inside the delete's own transaction (see the file comment). The
// sweeper owns them from here.
//
// The table is OPTIONAL to this statement: it arrives with the platform's
// shared migration 0162, and a deployment on the baseline alone (the Python
// indexing runtime with no shared migrations applied) must still delete
// toolkits. So the transaction first asks the catalogue whether the table
// exists (to_regclass, NULL when it or its schema does not) and skips the
// update when it is absent — the way #1255's toolkit journal guards the
// tables it only sometimes has (toolkitcleanup.Journal.toolkitRowExists).
// Both reads are in the delete's transaction, so a table that appears between
// them is impossible.
func tombstoneToolkitIndexes(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64) error {
	var present bool
	if err := tx.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.index_registry') IS NOT NULL`,
	).Scan(&present); err != nil {
		return fmt.Errorf("look for the index registry table: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.index_registry
SET deleted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE project_id = $1 AND toolkit_id = $2 AND deleted_at IS NULL`, projectID, toolkitID); err != nil {
		return fmt.Errorf("tombstone the toolkit's index registry rows: %w", err)
	}
	return nil
}
