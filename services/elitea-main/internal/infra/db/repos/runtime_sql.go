package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type sqlRow interface {
	Scan(dest ...any) error
}

type sqlRows interface {
	Close()
	Err() error
	Next() bool
	Scan(dest ...any) error
}

type sqlExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) sqlRow
	Query(ctx context.Context, sql string, args ...any) (sqlRows, error)
}

type pgxQueryer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type pgxExecutor struct {
	queryer pgxQueryer
}

var _ currentAgentTerminalWriter = pgxExecutor{}

func (e pgxExecutor) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return e.queryer.Exec(ctx, sql, arguments...)
}

func (e pgxExecutor) QueryRow(ctx context.Context, sql string, args ...any) sqlRow {
	return e.queryer.QueryRow(ctx, sql, args...)
}

func (e pgxExecutor) Query(ctx context.Context, sql string, args ...any) (sqlRows, error) {
	return e.queryer.Query(ctx, sql, args...)
}

func (e pgxExecutor) InsertCurrentIndexTerminalNotification(
	ctx context.Context,
	arg sqlcgen.InsertCurrentIndexTerminalNotificationParams,
) (int64, error) {
	return sqlcgen.New(e.queryer).InsertCurrentIndexTerminalNotification(ctx, arg)
}

func (e pgxExecutor) GetAgentExecutionTerminalNodeEvent(
	ctx context.Context,
	arg sqlcgen.GetAgentExecutionTerminalNodeEventParams,
) (sqlcgen.GetAgentExecutionTerminalNodeEventRow, error) {
	return sqlcgen.New(e.queryer).GetAgentExecutionTerminalNodeEvent(ctx, arg)
}

func (e pgxExecutor) GetCurrentAgentTraceBinding(
	ctx context.Context,
	arg sqlcgen.GetCurrentAgentTraceBindingParams,
) (sqlcgen.GetCurrentAgentTraceBindingRow, error) {
	return sqlcgen.New(e.queryer).GetCurrentAgentTraceBinding(ctx, arg)
}

func (e pgxExecutor) LockCurrentAgentResponseForTerminal(
	ctx context.Context,
	arg sqlcgen.LockCurrentAgentResponseForTerminalParams,
) (int32, error) {
	return sqlcgen.New(e.queryer).LockCurrentAgentResponseForTerminal(ctx, arg)
}

func (e pgxExecutor) InsertCurrentAgentTextItem(
	ctx context.Context,
	messageGroupID int64,
) (int32, error) {
	return sqlcgen.New(e.queryer).InsertCurrentAgentTextItem(ctx, messageGroupID)
}

func (e pgxExecutor) InsertCurrentAgentTextContent(
	ctx context.Context,
	arg sqlcgen.InsertCurrentAgentTextContentParams,
) error {
	return sqlcgen.New(e.queryer).InsertCurrentAgentTextContent(ctx, arg)
}

func (e pgxExecutor) DeleteCurrentAgentProvisionalText(
	ctx context.Context,
	arg sqlcgen.DeleteCurrentAgentProvisionalTextParams,
) error {
	return sqlcgen.New(e.queryer).DeleteCurrentAgentProvisionalText(ctx, arg)
}

func (e pgxExecutor) UpdateCurrentAgentAttachmentContent(
	ctx context.Context,
	arg sqlcgen.UpdateCurrentAgentAttachmentContentParams,
) (int64, error) {
	return sqlcgen.New(e.queryer).UpdateCurrentAgentAttachmentContent(ctx, arg)
}

func (e pgxExecutor) FinalizeCurrentAgentFullMessage(
	ctx context.Context,
	arg sqlcgen.FinalizeCurrentAgentFullMessageParams,
) (int64, error) {
	return sqlcgen.New(e.queryer).FinalizeCurrentAgentFullMessage(ctx, arg)
}

func (e pgxExecutor) FinalizeCurrentAgentHITLPause(
	ctx context.Context,
	arg sqlcgen.FinalizeCurrentAgentHITLPauseParams,
) (int64, error) {
	return sqlcgen.New(e.queryer).FinalizeCurrentAgentHITLPause(ctx, arg)
}

func (e pgxExecutor) FinalizeCurrentAgentAuthorizationPause(
	ctx context.Context,
	arg sqlcgen.FinalizeCurrentAgentAuthorizationPauseParams,
) (int64, error) {
	return sqlcgen.New(e.queryer).FinalizeCurrentAgentAuthorizationPause(ctx, arg)
}

func (e pgxExecutor) GetCurrentAgentInvokedSkills(
	ctx context.Context,
	messageGroupID int64,
) (string, error) {
	return sqlcgen.New(e.queryer).GetCurrentAgentInvokedSkills(ctx, messageGroupID)
}

func (e pgxExecutor) ResolveCurrentApplicationTurn(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentApplicationTurnParams,
) (sqlcgen.ResolveCurrentApplicationTurnRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentApplicationTurn(ctx, arg)
}

func (e pgxExecutor) ResolveCurrentApplicationVersionDetails(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentApplicationVersionDetailsParams,
) (sqlcgen.ResolveCurrentApplicationVersionDetailsRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentApplicationVersionDetails(ctx, arg)
}

func (e pgxExecutor) ResolveCurrentApplicationNestingNode(
	ctx context.Context,
	applicationVersionID int32,
) (sqlcgen.ResolveCurrentApplicationNestingNodeRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentApplicationNestingNode(ctx, applicationVersionID)
}

func (e pgxExecutor) ResolveCurrentAdhocTurn(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentAdhocTurnParams,
) (sqlcgen.ResolveCurrentAdhocTurnRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentAdhocTurn(ctx, arg)
}

func (e pgxExecutor) CurrentConversationResponseSettling(
	ctx context.Context,
	conversationUUID pgtype.UUID,
) (bool, error) {
	return sqlcgen.New(e.queryer).CurrentConversationResponseSettling(ctx, conversationUUID)
}

func (e pgxExecutor) ResolveCurrentRegeneration(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentRegenerationParams,
) (sqlcgen.ResolveCurrentRegenerationRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentRegeneration(ctx, arg)
}

func (e pgxExecutor) ResolveCurrentContinuation(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentContinuationParams,
) (sqlcgen.ResolveCurrentContinuationRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentContinuation(ctx, arg)
}

func (e pgxExecutor) ResolveCurrentOutputLimitContinuation(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentOutputLimitContinuationParams,
) (sqlcgen.ResolveCurrentOutputLimitContinuationRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentOutputLimitContinuation(ctx, arg)
}

func (e pgxExecutor) ResolveCurrentAuthorizationContinuation(
	ctx context.Context,
	arg sqlcgen.ResolveCurrentAuthorizationContinuationParams,
) (sqlcgen.ResolveCurrentAuthorizationContinuationRow, error) {
	return sqlcgen.New(e.queryer).ResolveCurrentAuthorizationContinuation(ctx, arg)
}

func (e pgxExecutor) GetCurrentAgentTaskStatus(
	ctx context.Context,
	arg sqlcgen.GetCurrentAgentTaskStatusParams,
) (sqlcgen.GetCurrentAgentTaskStatusRow, error) {
	return sqlcgen.New(e.queryer).GetCurrentAgentTaskStatus(ctx, arg)
}

func (e pgxExecutor) CancelCurrentAgentExecution(
	ctx context.Context,
	arg sqlcgen.CancelCurrentAgentExecutionParams,
) (sqlcgen.CancelCurrentAgentExecutionRow, error) {
	return sqlcgen.New(e.queryer).CancelCurrentAgentExecution(ctx, arg)
}

func (e pgxExecutor) ProjectCurrentAgentStop(
	ctx context.Context,
	arg sqlcgen.ProjectCurrentAgentStopParams,
) (sqlcgen.ProjectCurrentAgentStopRow, error) {
	return sqlcgen.New(e.queryer).ProjectCurrentAgentStop(ctx, arg)
}

func (e pgxExecutor) IsCurrentAgentCancellationReplay(
	ctx context.Context,
	arg sqlcgen.IsCurrentAgentCancellationReplayParams,
) (bool, error) {
	return sqlcgen.New(e.queryer).IsCurrentAgentCancellationReplay(ctx, arg)
}

type sharedStore interface {
	sqlExecutor
	WithinTx(ctx context.Context, opts pgx.TxOptions, fn func(sqlExecutor) error) error
}

type postgresSharedStore struct {
	pool *pgxpool.Pool
}

func newPostgresSharedStore(pool *pgxpool.Pool) (*postgresSharedStore, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	return &postgresSharedStore{pool: pool}, nil
}

func (s *postgresSharedStore) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return s.pool.Exec(ctx, sql, arguments...)
}

func (s *postgresSharedStore) QueryRow(ctx context.Context, sql string, args ...any) sqlRow {
	return s.pool.QueryRow(ctx, sql, args...)
}

func (s *postgresSharedStore) Query(ctx context.Context, sql string, args ...any) (sqlRows, error) {
	return s.pool.Query(ctx, sql, args...)
}

func (s *postgresSharedStore) WithinTx(ctx context.Context, opts pgx.TxOptions, fn func(sqlExecutor) error) (runErr error) {
	if fn == nil {
		return errors.New("transaction callback is required")
	}
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := fn(pgxExecutor{queryer: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

type projectStore interface {
	WithinProjectTx(ctx context.Context, projectID int64, opts pgx.TxOptions, fn func(sqlExecutor) error) error
}

type postgresProjectStore struct {
	executor *tenant.Executor
}

func newPostgresProjectStore(pool *pgxpool.Pool) (*postgresProjectStore, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	return &postgresProjectStore{executor: tenant.NewExecutor(pool)}, nil
}

func (s *postgresProjectStore) WithinProjectTx(ctx context.Context, projectID int64, opts pgx.TxOptions, fn func(sqlExecutor) error) error {
	if fn == nil {
		return errors.New("project transaction callback is required")
	}
	return s.executor.WithinTx(ctx, tenant.Project{ID: projectID}, opts, func(ctx context.Context, tx pgx.Tx) error {
		return fn(pgxExecutor{queryer: tx})
	})
}

func parseProjectID(value string) (int64, error) {
	projectID, err := strconv.ParseInt(value, 10, 64)
	if err != nil || projectID <= 0 || strconv.FormatInt(projectID, 10) != value {
		return 0, errors.New("project ID must be a canonical positive integer")
	}
	return projectID, nil
}
