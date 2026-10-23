package repos

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCurrentIndexCancellationRepositoryBindsExactActiveTarget(t *testing.T) {
	t.Parallel()

	database := &currentIndexCancelDB{row: scriptedRow{values: []any{true}}}
	repository, err := newCurrentIndexCancellationRepository(sqlcgen.New(database))
	if err != nil {
		t.Fatal(err)
	}
	request := indexingapp.CurrentIndexCancelRequest{
		ProjectID:   7,
		ToolkitID:   9,
		IndexName:   "documents",
		ExecutionID: "0123456789abcdef0123456789abcdef",
	}
	transitioned, err := repository.RequestCurrentIndexCancellation(context.Background(), request)
	if err != nil || !transitioned {
		t.Fatalf("RequestCurrentIndexCancellation() = %v, %v", transitioned, err)
	}
	if len(database.calls) != 1 {
		t.Fatalf("query calls=%d want=1", len(database.calls))
	}
	call := database.calls[0]
	wantArguments := []any{request.ExecutionID, int32(7), int32(9), request.IndexName}
	if !reflect.DeepEqual(call.args, wantArguments) {
		t.Fatalf("query arguments=%#v want=%#v", call.args, wantArguments)
	}
	for _, predicate := range []string{
		"job.execution_id = $1::text",
		"job.tenant_id = ($2::integer)::text",
		"job.resource_project_id = $2::integer",
		"job.projection_project_id = $2::integer",
		"job.capability_id = 'index.ingest.v1'",
		"job.desired_state = 'RUNNING'",
		"job.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')",
		"ingest.execution_id = job.execution_id",
		"ingest.generation = job.generation",
		"ingest.capability_id = job.capability_id",
		"ingest.toolkit_id = $3::integer",
		"ingest.index_name = $4::text",
		"SET index_manual_stop_requested_at = clock_timestamp()",
		"index_manual_cleanup_status = 'PENDING'",
		"index_manual_cleanup_attempt_count = 0",
		"ingest.index_meta_initialized_at IS NOT NULL",
		"ingest.index_manual_cleanup_status IS NULL",
		"SELECT EXISTS (SELECT 1 FROM transitioned)",
	} {
		if !strings.Contains(call.sql, predicate) {
			t.Errorf("generated cancellation query missing %q:\n%s", predicate, call.sql)
		}
	}
	transitionEnd := strings.Index(call.sql, "RETURNING job.execution_id")
	initializationGate := strings.Index(
		call.sql,
		"ingest.index_meta_initialized_at IS NOT NULL",
	)
	if transitionEnd < 0 || initializationGate < transitionEnd {
		t.Fatal(
			"metadata initialization incorrectly gates the cancellation transition",
		)
	}
}

func TestCurrentIndexCancellationRepositoryPreservesIdempotentNoTransition(t *testing.T) {
	t.Parallel()

	database := &currentIndexCancelDB{row: scriptedRow{values: []any{false}}}
	repository, err := newCurrentIndexCancellationRepository(sqlcgen.New(database))
	if err != nil {
		t.Fatal(err)
	}
	transitioned, err := repository.RequestCurrentIndexCancellation(
		context.Background(),
		indexingapp.CurrentIndexCancelRequest{
			ProjectID:   7,
			ToolkitID:   9,
			IndexName:   "documents",
			ExecutionID: "0123456789abcdef0123456789abcdef",
		},
	)
	if err != nil || transitioned {
		t.Fatalf("RequestCurrentIndexCancellation() = %v, %v; want false, nil", transitioned, err)
	}
}

func TestCurrentIndexCancellationRepositoryRejectsBeforeQuery(t *testing.T) {
	t.Parallel()

	valid := indexingapp.CurrentIndexCancelRequest{
		ProjectID:   7,
		ToolkitID:   9,
		IndexName:   "documents",
		ExecutionID: "0123456789abcdef0123456789abcdef",
	}
	for name, request := range map[string]indexingapp.CurrentIndexCancelRequest{
		"empty":            {},
		"project overflow": withCancelProjectID(valid, math.MaxInt32+1),
		"toolkit overflow": withCancelToolkitID(valid, math.MaxInt32+1),
	} {
		t.Run(name, func(t *testing.T) {
			database := &currentIndexCancelDB{row: scriptedRow{values: []any{true}}}
			repository, err := newCurrentIndexCancellationRepository(sqlcgen.New(database))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.RequestCurrentIndexCancellation(
				context.Background(),
				request,
			); !errors.Is(err, indexingapp.ErrInvalidCurrentIndexCancel) {
				t.Fatalf("RequestCurrentIndexCancellation() error=%v", err)
			}
			if len(database.calls) != 0 {
				t.Fatalf("invalid request issued %d queries", len(database.calls))
			}
		})
	}
}

func withCancelProjectID(
	request indexingapp.CurrentIndexCancelRequest,
	projectID int64,
) indexingapp.CurrentIndexCancelRequest {
	request.ProjectID = projectID
	return request
}

func withCancelToolkitID(
	request indexingapp.CurrentIndexCancelRequest,
	toolkitID int64,
) indexingapp.CurrentIndexCancelRequest {
	request.ToolkitID = toolkitID
	return request
}

type currentIndexCancelDB struct {
	row   scriptedRow
	calls []queryCall
}

func (database *currentIndexCancelDB) QueryRow(
	_ context.Context,
	query string,
	arguments ...any,
) pgx.Row {
	database.calls = append(database.calls, queryCall{
		sql:  query,
		args: append([]any(nil), arguments...),
	})
	return database.row
}

func (*currentIndexCancelDB) Query(
	context.Context,
	string,
	...any,
) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (*currentIndexCancelDB) Exec(
	context.Context,
	string,
	...any,
) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}
