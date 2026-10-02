package repos

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestPostgresCurrentAgentProjectContextSnapshotAndAdmission(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	_, err := pool.Exec(t.Context(), `INSERT INTO p_1.configuration (id,uuid,project_id,elitea_title,type,section,data,meta,shared,status_ok,source,author_id) VALUES (2,'60000000-0000-4000-8000-000000000039',1,'project_context_snapshot','project_context','project','{"content":" Original context ","activation_description":"when relevant"}'::jsonb,'{}'::jsonb,false,true,'user',11)`)
	require.NoError(t, err)
	repo := NewCurrentAgentProjectContextRepository(pool)
	first, err := repo.ResolveCurrentAgentProjectContext(t.Context(), 1, 11)
	require.NoError(t, err)
	require.Equal(t, int32(2), first.ID)
	require.True(t, first.Enabled)
	require.Equal(t, " Original context ", first.Content)
	require.Equal(t, "when relevant", first.ActivationDescription)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	require.NoError(t, tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}))
	_, err = sqlcgen.New(tx).ResolveCurrentApplicationTurn(t.Context(), sqlcgen.ResolveCurrentApplicationTurnParams{ActorUserID: 11, TargetParticipantID: 21, ProjectID: 1, QuestionID: mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000031"), ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")})
	require.NoError(t, err, "enabled project context must not block application admission")
	require.NoError(t, tx.Rollback(t.Context()))
	_, err = pool.Exec(t.Context(), `UPDATE p_1.configuration SET data='{"content":"New context","enabled":false}'::jsonb WHERE id=2`)
	require.NoError(t, err)
	second, err := repo.ResolveCurrentAgentProjectContext(t.Context(), 1, 11)
	require.NoError(t, err)
	require.False(t, second.Enabled)
	require.Equal(t, "New context", second.Content)
	require.Equal(t, " Original context ", first.Content, "previous snapshot must stay unchanged")
	_, err = repo.ResolveCurrentAgentProjectContext(t.Context(), 1, 0)
	require.Error(t, err)
}
