package repos

// The amendment of 2026-10-15 (ADR-0034 decision 5, contract §6) against a
// REAL database and the REAL project vault: explicit includes and the env
// harvest at send, the one hidden per-thread secret, its erasure on cancel,
// on erase and at release + grace, the claim-bound redeem, and an audit that
// names variables but never carries a value.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const harvestedValue = "postgres://user:s3cr3t-value@db/app"

func newEnvHarness(t *testing.T) (handoffHarness, *WorkspaceEnvVault, *storage.PostgresSecretVaultLoader) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentMutationVault(t, pool)
	return newEnvHarnessOn(t, pool)
}

func newEnvHarnessOn(t *testing.T, pool *pgxpool.Pool) (handoffHarness, *WorkspaceEnvVault, *storage.PostgresSecretVaultLoader) {
	t.Helper()
	writer, err := NewCurrentSecretVaultRepository(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Destroy)
	vault, err := NewWorkspaceEnvVault(writer, reader, v2secrets.NewHandler(pool))
	if err != nil {
		t.Fatal(err)
	}
	h := newHandoffHarness(t, pool)
	h.service.WithEnvVault(vault)
	h.policy.set(func(p *workspacehandoff.Policy) {
		p.EnvHarvest = true
		p.AllowUserInclude = true
		p.EnvDeny = []string{"PATH", "SSH_*"}
		p.PathDeny = []string{"**/.env.production"}
	})
	return h, vault, reader
}

func secretPresent(t *testing.T, reader *storage.PostgresSecretVaultLoader, name string) bool {
	t.Helper()
	vault, err := reader.LoadProjectVault(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = vault.Lookup(name)
	if err != nil && !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestPostgresWorkspaceEnvLifecycle(t *testing.T) {
	h, _, reader := newEnvHarness(t)
	ctx := context.Background()
	const owner int64 = 5401
	fixture := seedLocalWorkThread(t, h.pool, owner, "local_work")
	thread := fixture.thread

	// The amendment's refusals at send.
	refused := beginRequest(thread, owner)
	refused.IncludedExcluded = []string{"./api/.env.production"}
	if _, err := h.service.Begin(ctx, refused); !errors.Is(err, workspacehandoff.ErrPathDenied) {
		t.Fatalf("a path_deny include = %v", err)
	}
	refused = beginRequest(thread, owner)
	refused.Env = map[string]string{"SSH_AUTH_SOCK": "/tmp/agent", "DATABASE_URL": harvestedValue}
	var denied *workspacehandoff.EnvNameDeniedError
	if _, err := h.service.Begin(ctx, refused); !errors.As(err, &denied) || denied.Names[0] != "SSH_AUTH_SOCK" {
		t.Fatalf("a denied name = %v", err)
	}
	h.policy.set(func(p *workspacehandoff.Policy) { p.EnvHarvest = false })
	refused.Env = map[string]string{"DATABASE_URL": harvestedValue}
	if _, err := h.service.Begin(ctx, refused); !errors.Is(err, workspacehandoff.ErrEnvHarvestDisabled) {
		t.Fatalf("env with the harvest off = %v", err)
	}
	h.policy.set(func(p *workspacehandoff.Policy) { p.EnvHarvest = true; p.AllowUserInclude = false })
	refused = beginRequest(thread, owner)
	refused.IncludedExcluded = []string{"./.env"}
	if _, err := h.service.Begin(ctx, refused); !errors.Is(err, workspacehandoff.ErrIncludeNotAllowed) {
		t.Fatalf("an include with the policy off = %v", err)
	}
	h.policy.set(func(p *workspacehandoff.Policy) { p.AllowUserInclude = true })

	// A send with an include and the environment.
	send := beginRequest(thread, owner)
	send.IncludedExcluded = []string{"./.env"}
	send.Env = map[string]string{"DATABASE_URL": harvestedValue, "API_TOKEN": "tok-12345"}
	began, err := h.service.Begin(ctx, send)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	name := began.Handoff.EnvSecretName
	if !began.Handoff.EnvSecret() || !strings.HasPrefix(name, "workspace_env_") ||
		strings.Join(began.Handoff.EnvNames, ",") != "API_TOKEN,DATABASE_URL" ||
		strings.Join(began.Handoff.IncludedExcluded, ",") != "./.env" {
		t.Fatalf("begin = %+v", began.Handoff)
	}
	if !secretPresent(t, reader, name) {
		t.Fatalf("the env secret was not written")
	}
	// No value is on the row, and the audit names the variables only.
	var row string
	_ = h.pool.QueryRow(ctx, `SELECT row_to_json(h)::text FROM elitea_runtime.workspace_handoffs h WHERE conversation_uuid = $1::uuid`,
		thread.ConversationUUID).Scan(&row)
	if strings.Contains(row, "s3cr3t") || strings.Contains(row, "tok-12345") {
		t.Fatalf("the handoff row holds a value: %s", row)
	}
	audit := strings.Join(h.recorder.actions(), "\n")
	if !strings.Contains(audit, "env names (API_TOKEN DATABASE_URL)") || !strings.Contains(audit, "1 included (./.env)") ||
		strings.Contains(audit, "s3cr3t") || strings.Contains(audit, "tok-12345") {
		t.Fatalf("audit = %s", audit)
	}

	// The redeem answers only the running turn of the claim's epoch.
	sha := h.publishSnapshot(t, thread, owner, 1, "env")
	if _, err := h.service.Complete(ctx, workspacehandoff.CompleteRequest{ProjectID: 1, HandoffID: began.Handoff.HandoffID,
		ActorUserID: owner, Epoch: 1, HandoffRef: workspacestore.RefHandoff1, SHA: sha}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CloudTurnEnv(ctx, thread, 2, "exec-env"); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("a redeem with no running turn = %v", err)
	}
	if _, _, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-env", "5d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d"); err != nil {
		t.Fatal(err)
	}
	env, err := h.service.CloudTurnEnv(ctx, thread, 2, "exec-env")
	if err != nil || env["DATABASE_URL"] != harvestedValue || env["API_TOKEN"] != "tok-12345" || len(env) != 2 {
		t.Fatalf("redeem = %v, %v", env, err)
	}
	if _, err := h.service.CloudTurnEnv(ctx, thread, 2, "another-turn"); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("another turn's redeem = %v", err)
	}
	if _, err := h.service.FinishCloudTurn(ctx, thread, 2, "exec-env"); err != nil {
		t.Fatal(err)
	}

	// Take back and release: the secret stays for the return grace, then the
	// sweeper erases it with the objects.
	if _, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{Thread: thread, ActorUserID: owner, Epoch: 3, Outcome: "applied"}); err != nil {
		t.Fatal(err)
	}
	if !secretPresent(t, reader, name) {
		t.Fatalf("the env secret went before the return grace")
	}
	if _, err := h.pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET delete_after = clock_timestamp() - interval '1 second'
WHERE conversation_uuid = $1::uuid`, thread.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	h.service.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	if report, err := h.service.Sweep(ctx); err != nil || report.ObjectsErased != 1 {
		t.Fatalf("sweep = %+v, %v", report, err)
	}
	if secretPresent(t, reader, name) {
		t.Fatalf("the env secret outlived the return grace")
	}
	view, _ := h.service.State(ctx, thread)
	if view.Handoff.EnvSecretName != "" || len(view.Handoff.EnvNames) != 0 {
		t.Fatalf("an erased thread keeps its env: %+v", view.Handoff)
	}
}

func TestPostgresWorkspaceEnvErasedOnCancelAndEraseAndReplacedOrCleared(t *testing.T) {
	h, _, reader := newEnvHarness(t)
	ctx := context.Background()
	const owner int64 = 5411
	thread := seedLocalWorkThread(t, h.pool, owner, "local_work").thread
	send := beginRequest(thread, owner)
	send.Env = map[string]string{"DATABASE_URL": harvestedValue}

	// Cancel (DELETE in HANDING_OFF) erases the secret.
	began, err := h.service.Begin(ctx, send)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	if secretPresent(t, reader, began.Handoff.EnvSecretName) {
		t.Fatalf("a cancelled send kept its env secret")
	}

	// A later send without `env` keeps the previous secret; one with a new
	// env replaces it; `env: {}` clears it.
	first, err := h.service.Begin(ctx, send)
	if err != nil {
		t.Fatal(err)
	}
	h.publishSnapshot(t, thread, owner, first.Handoff.Epoch, "a")
	if _, err := h.service.Complete(ctx, workspacehandoff.CompleteRequest{ProjectID: 1, HandoffID: first.Handoff.HandoffID,
		ActorUserID: owner, Epoch: first.Handoff.Epoch, HandoffRef: workspacestore.RefHandoff1, SHA: sha1Of("snapshot a")}); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: first.Handoff.Epoch + 1})
	if err != nil {
		t.Fatal(err)
	}
	released, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{Thread: thread, ActorUserID: owner, Epoch: reclaimed.Epoch, Outcome: "applied"})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil || kept.Handoff.EnvSecretName != first.Handoff.EnvSecretName || !secretPresent(t, reader, first.Handoff.EnvSecretName) {
		t.Fatalf("a send without env = %+v, %v (released at %d)", kept.Handoff, err, released.Epoch)
	}
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	if secretPresent(t, reader, first.Handoff.EnvSecretName) {
		t.Fatalf("an erased thread kept its env secret")
	}

	replaced, err := h.service.Begin(ctx, send)
	if err != nil {
		t.Fatal(err)
	}
	again := send
	again.Env = map[string]string{"API_TOKEN": "tok-99999"}
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Begin(ctx, again)
	if err != nil || second.Handoff.EnvSecretName == replaced.Handoff.EnvSecretName {
		t.Fatalf("a new env did not replace the secret: %+v, %v", second.Handoff, err)
	}
	cleared := beginRequest(thread, owner)
	cleared.Env = map[string]string{}
	h.policy.set(func(p *workspacehandoff.Policy) { p.EnvHarvest = false })
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	third, err := h.service.Begin(ctx, cleared)
	if err != nil || third.Handoff.EnvSecret() || secretPresent(t, reader, second.Handoff.EnvSecretName) {
		t.Fatalf("env: {} did not clear the secret: %+v, %v", third.Handoff, err)
	}
}

// A project with no vault (the default project of a fresh install) gets one
// created on its first send with env, instead of a 503.
func TestPostgresWorkspaceEnvCreatesAnAbsentProjectVault(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
CREATE TABLE centry.secrets_key (id TEXT PRIMARY KEY, data BYTEA);
CREATE TABLE centry.secrets_data (id TEXT PRIMARY KEY, data BYTEA);`); err != nil {
		t.Fatal(err)
	}
	h, _, reader := newEnvHarnessOn(t, pool)
	const owner int64 = 5421
	thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
	send := beginRequest(thread, owner)
	send.Env = map[string]string{"DATABASE_URL": harvestedValue}
	began, err := h.service.Begin(ctx, send)
	if err != nil {
		t.Fatalf("a send with env into a project with no vault = %v", err)
	}
	if !secretPresent(t, reader, began.Handoff.EnvSecretName) {
		t.Fatalf("the env secret is not in the created vault")
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM centry.secrets_key WHERE id = 'project-1'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("vault key rows = %d, want the created one", rows)
	}
}
