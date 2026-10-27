package credentialbroker

// Shared 0188 (issue #1118): the static-secret happy path over the real
// store and audit outbox, the replay rule on the bound invocation row, the
// agent-session link, and an audit failure rolling the invocation back.
// CI-only: skipped without the integration database.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	brokerURLEnv    = "ELITEA_TEST_DATABASE_URL"
	brokerBootstrap = "../infra/db/migrations/001_initial.sql"
)

var (
	brokerTemplateOnce sync.Once
	brokerTemplate     string
	brokerTemplateErr  error
)

func newBrokerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(brokerURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the credential broker integration test", brokerURLEnv)
	}
	brokerTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(brokerBootstrap)
		if err != nil {
			brokerTemplateErr = err
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			brokerTemplateErr = err
			return
		}
		defer admin.Close()
		brokerTemplate, brokerTemplateErr = dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{
			Files: platformmigrations.Files, Seed: string(bootstrap),
		})
	})
	require.NoError(t, brokerTemplateErr)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_broker_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, brokerTemplate, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// fixedCeiling keeps the real store but answers the session ceiling, which
// reads grant tables this test does not seed.
type fixedCeiling struct{ *PostgresStore }

func (fixedCeiling) SessionCeiling(context.Context, int64, string) ([]string, error) {
	return []string{"api://jira-a"}, nil
}

type failingAppender struct {
	inner securityaudit.Appender
	fail  securityaudit.EventType
}

func (a failingAppender) Append(ctx context.Context, tx pgx.Tx, e securityaudit.SecurityEvent) error {
	if e.Type == a.fail {
		return errors.New("outbox refused")
	}
	return a.inner.Append(ctx, tx, e)
}

func brokerOver(t *testing.T, w *world, pool *pgxpool.Pool, audit securityaudit.Appender) *Broker {
	t.Helper()
	store, err := NewPostgresStore(pool, audit)
	require.NoError(t, err)
	return w.broker(t, func(c *Config) { c.Store = fixedCeiling{store} })
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func TestPostgresStaticRedemption(t *testing.T) {
	pool := newBrokerPool(t)
	audit, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	require.NoError(t, err)
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	b := brokerOver(t, w, pool, audit)

	result := redeem(t, b, w.request(t))
	require.NotNil(t, result.Envelope, "%+v", result.Denial)
	require.Equal(t, tSecretValue, string(result.Envelope.Material["api_token"]))
	envelope := *result.Envelope
	result.Envelope.Destroy()

	var (
		decisionID, kind string
		notAfter         time.Time
	)
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT decision_id::text, kind, not_after FROM elitea_runtime.credential_redemptions WHERE redemption_id = $1::uuid`,
		envelope.RedemptionID).Scan(&decisionID, &kind, &notAfter))
	require.Equal(t, envelope.DecisionID, decisionID)
	require.Equal(t, "static", kind)
	require.True(t, notAfter.Equal(tNow.Add(MaxEnvelopeTTL)))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM elitea_runtime.bound_tool_invocations`))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`,
		string(securityaudit.TypeDecision)))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`,
		string(securityaudit.TypeCredentialRedeem)))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM elitea_runtime.execution_agent_sessions WHERE execution_id = $1`, tExecution))

	// No row and no event column holds the material.
	require.Zero(t, count(t, pool, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE row_to_json(security_audit_outbox)::text LIKE '%' || $1 || '%'`,
		tSecretValue))

	// An identical replay is re-evaluated and reuses the session.
	again := redeem(t, b, w.request(t))
	require.NotNil(t, again.Envelope)
	again.Envelope.Destroy()
	require.Len(t, w.sessions.opened, 1)
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM elitea_runtime.bound_tool_invocations`))
	require.Equal(t, 2, count(t, pool, `SELECT count(*) FROM elitea_runtime.credential_redemptions`))

	// A changed replay is INVOCATION_MISMATCH and reads nothing.
	reads := w.secrets.readCount()
	changed := w.request(t)
	changed.Invocation.ArgumentsDigest[0] ^= 0xff
	requireDenial(t, redeem(t, b, changed), CodeInvocationMismatch)
	require.Equal(t, reads, w.secrets.readCount())

	// The rows are immutable.
	_, err = pool.Exec(context.Background(), `UPDATE elitea_runtime.bound_tool_invocations SET resource_ref = 'x'`)
	require.Error(t, err)
}

func TestPostgresAuditFailureRollsTheInvocationBack(t *testing.T) {
	pool := newBrokerPool(t)
	inner, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	require.NoError(t, err)

	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	b := brokerOver(t, w, pool, failingAppender{inner: inner, fail: securityaudit.TypeDecision})
	requireDenial(t, redeem(t, b, w.request(t)), CodeDenied)
	require.Zero(t, w.secrets.readCount())
	require.Zero(t, count(t, pool, `SELECT count(*) FROM elitea_runtime.bound_tool_invocations`))

	w = newWorld().withMandate(mandate.KindDelegatedBackground)
	b = brokerOver(t, w, pool, failingAppender{inner: inner, fail: securityaudit.TypeCredentialRedeem})
	requireDenial(t, redeem(t, b, w.request(t)), CodeDenied)
	require.Zero(t, count(t, pool, `SELECT count(*) FROM elitea_runtime.credential_redemptions`))
}
