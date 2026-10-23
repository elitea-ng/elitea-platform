package tenant

import (
	"context"
	"fmt"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

// Project identifies an already authorized project. Callers cannot provide a
// database schema name.
type Project struct {
	ID int64
}

// Beginner is satisfied by pgxpool.Pool and keeps transaction ownership at
// this boundary.
type Beginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// Executor guarantees that tenant context exists only inside one transaction.
type Executor struct {
	db Beginner
}

func NewExecutor(db Beginner) *Executor {
	return &Executor{db: db}
}

// BindProject validates an already-authorized project and installs its current
// tenant schema on an existing transaction. It exists for cross-schema atomic
// operations, such as writing the current chat turn and elitea_runtime outbox
// in one commit. The project ID, never a caller-provided schema identifier, is
// the only input used to derive the search path.
func BindProject(ctx context.Context, tx pgx.Tx, project Project) error {
	if project.ID <= 0 {
		return fmt.Errorf("tenant: project ID must be positive")
	}
	if tx == nil {
		return fmt.Errorf("tenant: transaction is required")
	}

	schema := "p_" + strconv.FormatInt(project.ID, 10)
	queries := sqlcgen.New(tx)
	resolved, err := queries.ResolveCurrentTenantContext(
		ctx,
		sqlcgen.ResolveCurrentTenantContextParams{
			ProjectID:  project.ID,
			SchemaName: schema,
		},
	)
	if err != nil {
		return fmt.Errorf("tenant: resolve project: %w", err)
	}
	if !resolved.ProjectExists {
		return fmt.Errorf("tenant: project does not exist")
	}
	if !resolved.SchemaExists {
		return fmt.Errorf("tenant: schema %s does not exist", schema)
	}

	searchPath := pgx.Identifier{schema}.Sanitize()
	if _, err := queries.InstallCurrentTenantSearchPath(ctx, searchPath); err != nil {
		return fmt.Errorf("tenant: install transaction context: %w", err)
	}
	effectiveSchema, err := queries.GetCurrentTenantSchema(ctx)
	if err != nil {
		return fmt.Errorf("tenant: verify transaction context: %w", err)
	}
	if effectiveSchema != schema {
		return fmt.Errorf("tenant: schema %s is not the effective schema", schema)
	}
	return nil
}

// WithinTx verifies the project inside the transaction, derives the legacy
// p_<project-id> schema from the positive integer identity, installs a
// transaction-local search_path, executes fn, and always ends the transaction
// before returning. A raw pooled connection never escapes.
func (e *Executor) WithinTx(
	ctx context.Context,
	project Project,
	opts pgx.TxOptions,
	fn func(context.Context, pgx.Tx) error,
) (runErr error) {
	if project.ID <= 0 {
		return fmt.Errorf("tenant: project ID must be positive")
	}
	if fn == nil {
		return fmt.Errorf("tenant: transaction callback is required")
	}

	tx, err := e.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("tenant: begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := BindProject(ctx, tx, project); err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: commit transaction: %w", err)
	}
	return nil
}
