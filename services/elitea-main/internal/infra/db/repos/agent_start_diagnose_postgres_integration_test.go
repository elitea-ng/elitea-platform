package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
)

// The diagnostic that runs after ResolveCurrentApplicationTurn finds no row
// must name the FIRST condition that failed (#2020). Each case breaks exactly
// one thing in an otherwise healthy fixture and reads the refusal the sender
// would get; the last case is the healthy fixture itself, which has no failing
// condition and must fall back to the generic sentence rather than guess.
func TestPostgresDiagnoseCurrentApplicationTurnNamesTheFailedCondition(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		healthyConversation = "10000000-0000-4000-8000-000000000031"
		toolkitConversation = "10000000-0000-4000-8000-000000000032"
		unknownConversation = "10000000-0000-4000-8000-0000000000ff"
		question            = "20000000-0000-4000-8000-000000000031"
	)
	cases := []struct {
		name         string
		mutate       string
		conversation string
		actor        int64
		target       int32
		want         string
	}{
		{name: "conversation missing", conversation: unknownConversation, actor: 11, target: 21, want: "does not exist in this project"},
		{name: "caller not in conversation", conversation: healthyConversation, actor: 999, target: 21, want: "you are not a participant"},
		{name: "agent not in conversation", conversation: healthyConversation, actor: 11, target: 999, want: "is not part of this conversation"},
		{name: "participant is not an agent", conversation: healthyConversation, actor: 11, target: 20, want: "is not an agent"},
		{
			name:         "agent of another project",
			mutate:       `UPDATE p_1.chat_participants SET entity_meta = '{"id":31,"project_id":7}'::jsonb WHERE id = 21`,
			conversation: healthyConversation, actor: 11, target: 21, want: "belongs to another project",
		},
		{
			name:         "saved version gone",
			mutate:       `UPDATE p_1.chat_participant_mapping SET entity_settings = '{"version_id":999}'::jsonb WHERE participant_id = 21`,
			conversation: healthyConversation, actor: 11, target: 21, want: "no longer exists",
		},
		{
			name:         "conversation internal tool unknown",
			mutate:       `UPDATE p_1.chat_conversations SET meta = '{"internal_tools":["bogus"]}'::jsonb WHERE id = 1`,
			conversation: healthyConversation, actor: 11, target: 21, want: "this conversation enables an internal tool",
		},
		{
			name:         "agent internal tool unknown",
			mutate:       `UPDATE p_1.application_versions SET meta = '{"internal_tools":["bogus"]}'::jsonb WHERE id = 41`,
			conversation: healthyConversation, actor: 11, target: 21, want: "the agent enables an internal tool",
		},
		{
			name:         "conversation summarized",
			mutate:       `UPDATE p_1.chat_conversations SET meta = '{"context_analytics":{"last_summarization":{"summary_content":"earlier"}}}'::jsonb WHERE id = 1`,
			conversation: healthyConversation, actor: 11, target: 21, want: "was summarized",
		},
		{name: "toolkit participant", conversation: toolkitConversation, actor: 11, target: 30, want: "a toolkit is a participant"},
		{name: "nothing failing", conversation: healthyConversation, actor: 11, target: 21, want: "could not be matched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(t.Context()) }()
			if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != "" {
				if _, err := tx.Exec(t.Context(), tc.mutate); err != nil {
					t.Fatal(err)
				}
			}
			refusal := currentApplicationTurnRefusal(
				t.Context(), pgxExecutor{queryer: tx},
				mustCurrentPGUUID(t, tc.conversation), tc.actor, tc.target, 1, 2,
				mustCurrentPGUUID(t, question),
			)
			if !errors.Is(refusal, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
				t.Fatalf("refusal = %v, want an unsupported start", refusal)
			}
			reasons := agentexecutionapp.UnsupportedReasons(refusal)
			if len(reasons) != 1 || !strings.Contains(reasons[0], tc.want) {
				t.Fatalf("reasons = %q, want one containing %q", reasons, tc.want)
			}
		})
	}
}

// brokenDiagnosis runs a statement that fails in place of the diagnostic.
type brokenDiagnosis struct{ sqlExecutor }

func (b brokenDiagnosis) QueryRow(ctx context.Context, _ string, _ ...any) sqlRow {
	return b.sqlExecutor.QueryRow(ctx, "SELECT 1/0")
}

// A diagnostic that fails must not abort the caller's transaction (SQLSTATE
// 25P02 on every later statement): it runs in a savepoint and the refusal
// falls back to the generic sentence.
func TestPostgresDiagnoseFailureLeavesTheTransactionUsable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	refusal := currentApplicationTurnRefusal(
		t.Context(), brokenDiagnosis{pgxExecutor{queryer: tx}},
		mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"), 11, 21, 1, 2,
		mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000031"),
	)
	reasons := agentexecutionapp.UnsupportedReasons(refusal)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "could not be matched") {
		t.Fatalf("reasons = %q", reasons)
	}
	if _, err := tx.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatalf("the transaction is aborted after a failed diagnosis: %v", err)
	}
}
