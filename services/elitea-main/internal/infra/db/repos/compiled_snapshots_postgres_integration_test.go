package repos

import (
	"context"
	"errors"
	"fmt"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const compiledPGDSNEnv = "ELITEA_COMPILED_SNAPSHOT_PG_FIXTURE_DSN"
const compiledPGRequiredEnv = "ELITEA_COMPILED_SNAPSHOT_PG_REQUIRED"
const compiledPGAckEnv = "ELITEA_COMPILED_SNAPSHOT_PG_DISPOSABLE_ACK"

// No default DSN, host resolution, credential discovery, database creation or
// database drop. The owning CI job/operator creates an empty disposable DB.
type compiledPGFixtureSpec struct {
	DSN, Host, Database string
	MaxConns            int32
}

func compiledPGFixtureConfig(dsn, required, ack string) (*compiledPGFixtureSpec, bool, error) {
	if required != "" && required != "false" && required != "true" {
		return nil, false, errors.New("compiled snapshot PostgreSQL required mode must be true or false")
	}
	if dsn == "" {
		if required == "true" {
			return nil, false, errors.New("required compiled snapshot PostgreSQL fixture is missing")
		}
		return nil, true, nil
	}
	if ack != "isolated_fixture_only" {
		return nil, false, errors.New("compiled snapshot PostgreSQL fixture requires disposable acknowledgement")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || len(dsn) > 16384 || strings.ContainsAny(dsn, "\r\n\x00") || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" || parsed.Fragment != "" || parsed.RawPath != "" {
		return nil, false, errors.New("compiled snapshot PostgreSQL fixture URL is invalid")
	}
	addr, err := netip.ParseAddr(parsed.Hostname())
	if err != nil || !addr.IsLoopback() || parsed.Port() == "" {
		return nil, false, errors.New("compiled snapshot PostgreSQL fixture must use literal loopback and explicit port")
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if len(name) > 63 || !strings.HasPrefix(name, "elitea_compiled_fixture_") || len(name) == len("elitea_compiled_fixture_") {
		return nil, false, errors.New("compiled snapshot PostgreSQL fixture database name is not disposable")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return nil, false, errors.New("compiled snapshot PostgreSQL fixture database name is invalid")
		}
	}
	if parsed.RawQuery != "sslmode=disable" {
		return nil, false, errors.New("compiled snapshot PostgreSQL fixture only accepts fixed local TLS mode")
	}
	// Keep guards free of pgx's implicit libpq file reads.
	return &compiledPGFixtureSpec{DSN: dsn, Host: addr.String(), Database: name, MaxConns: 4}, false, nil
}
func TestCompiledSnapshotPostgresFixtureGuard(t *testing.T) {
	good := "postgres://fixture@127.0.0.1:5432/elitea_compiled_fixture_123?sslmode=disable"
	if config, skip, err := compiledPGFixtureConfig(good, "true", "isolated_fixture_only"); err != nil || skip || config.MaxConns != 4 {
		t.Fatal("valid synthetic fixture rejected")
	}
	if _, skip, err := compiledPGFixtureConfig("", "false", ""); err != nil || !skip {
		t.Fatal("local omission not explicit skip")
	}
	for _, tc := range []struct{ dsn, required, ack string }{{"", "true", ""}, {good, "yes", "isolated_fixture_only"}, {good, "true", ""}, {strings.Replace(good, "127.0.0.1", "localhost", 1), "true", "isolated_fixture_only"}, {strings.Replace(good, "127.0.0.1", "10.0.0.1", 1), "true", "isolated_fixture_only"}, {strings.Replace(good, "elitea_compiled_fixture_123", "business", 1), "true", "isolated_fixture_only"}, {good + "&host=10.0.0.1", "true", "isolated_fixture_only"}, {strings.Replace(good, ":5432", "", 1), "true", "isolated_fixture_only"}} {
		if _, _, err := compiledPGFixtureConfig(tc.dsn, tc.required, tc.ack); err == nil {
			t.Fatal("unsafe/missing PostgreSQL fixture accepted")
		}
	}
}

type compiledPGFixture struct {
	pool *pgxpool.Pool
	ctx  context.Context
}

func newCompiledPGFixture(t *testing.T) *compiledPGFixture {
	t.Helper()
	config, skip, err := compiledPGFixtureConfig(os.Getenv(compiledPGDSNEnv), os.Getenv(compiledPGRequiredEnv), os.Getenv(compiledPGAckEnv))
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skip("isolated PostgreSQL fixture omitted; owning CI requires ELITEA_COMPILED_SNAPSHOT_PG_REQUIRED=true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	// Only this acknowledged real fixture path constructs the owning driver.
	driverConfig, err := pgxpool.ParseConfig(config.DSN)
	if err != nil || driverConfig.ConnConfig.Host != config.Host || driverConfig.ConnConfig.Database != config.Database {
		t.Fatal("isolated PostgreSQL fixture connection is invalid")
	}
	driverConfig.MaxConns = config.MaxConns
	driverConfig.MinConns = 0
	driverConfig.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, driverConfig)
	if err != nil {
		t.Fatal("isolated PostgreSQL fixture pool unavailable")
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("isolated PostgreSQL fixture could not connect")
	}
	var actual string
	var userTables int
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&actual); err != nil || actual != config.Database {
		t.Fatal("isolated PostgreSQL fixture database mismatch")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','v','m','f')`).Scan(&userTables); err != nil || userTables != 0 {
		t.Fatal("isolated PostgreSQL fixture must be an empty disposable database")
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyAgentState(ctx); err != nil {
		t.Fatal("apply exact owning agentstate fixture history:", err)
	}
	if err := runner.CheckHead(ctx, migrate.ScopeAgentState, "agentstate"); err != nil {
		t.Fatal("owning agentstate fixture head:", err)
	}
	return &compiledPGFixture{pool, ctx}
}
func (f *compiledPGFixture) reset(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM elitea_runtime.rust_compiled_snapshots; DELETE FROM elitea_runtime.sandbox_dispatches; DELETE FROM elitea_runtime.sandbox_jobs`); err != nil {
		t.Fatal("reset guarded fixture rows:", err)
	}
}
func (f *compiledPGFixture) repository(t *testing.T, q SnapshotQuota) *CompiledSnapshotsRepository {
	t.Helper()
	r, err := NewCompiledSnapshotsRepository(f.pool, q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func compiledPGQuota() SnapshotQuota {
	return SnapshotQuota{GlobalEntries: 10, GlobalBytes: 1 << 28, TenantEntries: 5, TenantBytes: 1 << 27, PublishingTTL: time.Minute, ReadyTTL: time.Hour}
}
func (f *compiledPGFixture) capture(t *testing.T, tenant, seed string) domain.SnapshotCandidate {
	t.Helper()
	raw, err := os.ReadFile("testdata/compiled-snapshot-v1/descriptor.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := domain.ParseRustSnapshotDescriptor(raw, domain.SnapshotContentSHA256(raw))
	if err != nil {
		t.Fatal(err)
	}
	d.Binding.TenantID = tenant
	d.Binding.SourceSHA256 = domain.SnapshotContentSHA256([]byte(seed))
	d.Binding.BasePreparedRequestSHA256 = domain.SnapshotContentSHA256([]byte("prepared-" + seed))
	d.SnapshotKeySHA256, _ = d.Binding.Key()
	descriptor, _ := domain.SnapshotJSON(d)
	digest, _ := domain.SnapshotJobDigest("compile", d.Binding, "")
	c := domain.SnapshotCandidate{Scope: domain.SnapshotScope{TenantID: tenant, ProjectID: 1}, Key: d.SnapshotKeySHA256, Root: domain.SnapshotContentSHA256(descriptor), DescriptorJSON: descriptor, CompilationJobKey: domain.SnapshotContentSHA256([]byte("original-compile-" + seed)), CompilationRequestDigest: digest, CompilationRuntimeID: "fixture-runtime-" + seed, CompilationLeaseEpoch: 3, CurrentLeaseLive: true}
	_, err = f.pool.Exec(f.ctx, `INSERT INTO elitea_runtime.sandbox_jobs(tenant_id,project_id,job_key,request_digest,phase,owner_id,lease_epoch,lease_until,runtime_id,compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json,compiled_export_verified,compiled_export_lease_epoch) VALUES($1,$2,$3,$4,'dispatched','fixture-compiler',3,clock_timestamp()+interval '1 minute',$5,'compile',$6,$7,$8,true,3)`, tenant, int32(1), snapshotBytes(c.CompilationJobKey), snapshotBytes(digest), c.CompilationRuntimeID, snapshotBytes(c.Key), snapshotBytes(d.Binding.BasePreparedRequestSHA256), descriptor)
	if err != nil {
		t.Fatal("create original fixture compile:", err)
	}
	return c
}
func (f *compiledPGFixture) complete(t *testing.T, c domain.SnapshotCandidate, cleanup bool) domain.SnapshotCandidate {
	t.Helper()
	d, err := c.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	stdout, _ := domain.SnapshotJSON(struct {
		Revision int                           `json:"revision"`
		Artifact domain.RustSnapshotDescriptor `json:"compiled_artifact"`
	}{1, d})
	receipt, _ := domain.SnapshotJSON(map[string]any{"revision": 1, "status": "completed", "exit_code": 0, "stdout": string(stdout) + "\n", "stderr": ""})
	_, err = f.pool.Exec(f.ctx, `UPDATE elitea_runtime.sandbox_jobs SET phase='completed',result_json=$4,runtime_cleanup_confirmed_at=CASE WHEN $5 THEN clock_timestamp() ELSE NULL END WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(c.CompilationJobKey), string(receipt), cleanup)
	if err != nil {
		t.Fatal("complete fixture receipt:", err)
	}
	c.ReceiptSHA256 = domain.SnapshotContentSHA256(receipt)
	return c
}
func (f *compiledPGFixture) ready(t *testing.T, r *CompiledSnapshotsRepository, c domain.SnapshotCandidate) domain.SnapshotCandidate {
	t.Helper()
	if err := r.Reserve(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	c = f.complete(t, c, true)
	if err := r.CommitReady(f.ctx, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return c
}
func (f *compiledPGFixture) expire(t *testing.T, c domain.SnapshotCandidate) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE elitea_runtime.rust_compiled_snapshots SET expires_at=created_at+interval '1 microsecond' WHERE tenant_id=$1 AND project_id=$2 AND snapshot_key=$3`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(c.Key)); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledSnapshotPostgresLifecycle(t *testing.T) {
	f := newCompiledPGFixture(t)
	t.Run("released_compile_then_reclaimed_staging_and_successful_ready_CAS", func(t *testing.T) {
		f.reset(t)
		r := f.repository(t, compiledPGQuota())
		c := f.capture(t, "fixture-tenant", "a")
		_, err := f.pool.Exec(f.ctx, `UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp() WHERE job_key=$1`, snapshotBytes(c.CompilationJobKey))
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := r.Candidate(f.ctx, c.Scope, c.Key, c.CompilationJobKey, false)
		if err != nil || candidate.CurrentLeaseLive {
			t.Fatal("released capture cannot obtain initial Publish authority", err)
		}
		if err := r.Reserve(f.ctx, candidate); !errors.Is(err, domain.ErrSnapshotUnavailable) {
			t.Fatal("released compile staged content", err)
		}
		_, err = f.pool.Exec(f.ctx, `UPDATE elitea_runtime.sandbox_jobs SET owner_id='fixture-publisher',lease_epoch=4,lease_until=clock_timestamp()+interval '1 minute' WHERE job_key=$1`, snapshotBytes(c.CompilationJobKey))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(f.ctx, c); err != nil {
			t.Fatal("reservation not idempotent", err)
		}
		if _, err := r.Ready(f.ctx, c.Scope, c.Key); !errors.Is(err, domain.ErrSnapshotUnavailable) {
			t.Fatal("staged row readable", err)
		}
		if err := r.CommitReady(f.ctx, c, func() error { t.Fatal("premature ready callback"); return nil }); err == nil {
			t.Fatal("live capture promoted")
		}
		c = f.complete(t, c, false)
		if _, err := r.Candidate(f.ctx, c.Scope, c.Key, c.CompilationJobKey, true); !errors.Is(err, domain.ErrSnapshotUnavailable) {
			t.Fatal("cleanup omission accepted", err)
		}
		c = f.complete(t, c, true)
		badContent := errors.New("fixed executable verification failed")
		if err := r.CommitReady(f.ctx, c, func() error { return badContent }); !errors.Is(err, badContent) {
			t.Fatal("corrupt content committed", err)
		}
		if _, err := r.Ready(f.ctx, c.Scope, c.Key); !errors.Is(err, domain.ErrSnapshotUnavailable) {
			t.Fatal("failed callback became ready", err)
		}
		if err := r.CommitReady(f.ctx, c, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := r.CommitReady(f.ctx, c, func() error { t.Fatal("ready retry re-uploaded"); return nil }); err != nil {
			t.Fatal(err)
		}
		ready, err := r.Ready(f.ctx, c.Scope, c.Key)
		if err != nil || ready.CompilationLeaseEpoch != 3 || ready.ReceiptSHA256 != c.ReceiptSHA256 {
			t.Fatal("rotating publisher lease altered immutable provenance", err)
		}
		if err := r.WithReady(f.ctx, c.Scope, c.Key, strings.Repeat("a", 64), func(domain.SnapshotCandidate) error { t.Fatal("wrong root delivered"); return nil }); err == nil {
			t.Fatal("selected root substitution accepted")
		}
	})
	t.Run("concurrent_global_and_tenant_entry_quotas", func(t *testing.T) {
		f.reset(t)
		q := compiledPGQuota()
		q.GlobalEntries = 2
		q.TenantEntries = 1
		r := f.repository(t, q)
		r2 := f.repository(t, q)
		candidates := []domain.SnapshotCandidate{f.capture(t, "fixture-a", "a1"), f.capture(t, "fixture-a", "a2"), f.capture(t, "fixture-b", "b1"), f.capture(t, "fixture-b", "b2")}
		results := make(chan error, 4)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, c := range candidates {
			wg.Add(1)
			go func(i int, c domain.SnapshotCandidate) {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					results <- r.Reserve(f.ctx, c)
				} else {
					results <- r2.Reserve(f.ctx, c)
				}
			}(i, c)
		}
		close(start)
		wg.Wait()
		close(results)
		success, quota := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, domain.ErrSnapshotQuota) {
				quota++
			} else {
				t.Fatal(err)
			}
		}
		if success != 2 || quota != 2 {
			t.Fatal("concurrent quota overshoot", success, quota)
		}
	})
	t.Run("global_and_tenant_byte_quotas", func(t *testing.T) {
		f.reset(t)
		a := f.capture(t, "fixture-a", "a")
		b := f.capture(t, "fixture-b", "b")
		da, _ := a.Descriptor()
		db, _ := b.Descriptor()
		sa := int64(len(a.DescriptorJSON)) + da.ExecutableBytes
		sb := int64(len(b.DescriptorJSON)) + db.ExecutableBytes
		q := compiledPGQuota()
		q.GlobalBytes = sa + sb - 1
		q.TenantBytes = q.GlobalBytes
		r := f.repository(t, q)
		if err := r.Reserve(f.ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(f.ctx, b); !errors.Is(err, domain.ErrSnapshotQuota) {
			t.Fatal("global bytes overshot", err)
		}
		f.reset(t)
		a = f.capture(t, "fixture-a", "a")
		q = compiledPGQuota()
		q.TenantBytes = sa - 1
		r = f.repository(t, q)
		if err := r.Reserve(f.ctx, a); !errors.Is(err, domain.ErrSnapshotQuota) {
			t.Fatal("tenant bytes overshot", err)
		}
	})
	t.Run("cancelled_or_changed_original_proof_cannot_stage", func(t *testing.T) {
		for _, mutation := range []string{"cancellation_requested=true", "request_digest=decode(repeat('a',64),'hex')", "runtime_id='changed-original-runtime'", "compiled_base_request_digest=decode(repeat('a',64),'hex')", "compiled_export_verified=false,compiled_export_lease_epoch=NULL"} {
			f.reset(t)
			r := f.repository(t, compiledPGQuota())
			c := f.capture(t, "fixture-a", "a")
			if _, err := f.pool.Exec(f.ctx, `UPDATE elitea_runtime.sandbox_jobs SET `+mutation+` WHERE job_key=$1`, snapshotBytes(c.CompilationJobKey)); err != nil {
				t.Fatal(err)
			}
			if err := r.Reserve(f.ctx, c); err == nil {
				t.Fatal("changed original proof authorized staging")
			}
		}
	})
	t.Run("expiry_reclaims_failed_upload_and_stale_deleter_is_fenced", func(t *testing.T) {
		f.reset(t)
		q := compiledPGQuota()
		q.GlobalEntries = 1
		q.TenantEntries = 1
		r := f.repository(t, q)
		c := f.capture(t, "fixture-a", "a")
		if err := r.Reserve(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		f.expire(t, c)
		if err := r.WithPublishing(f.ctx, c, func() error { t.Fatal("expired upload reached backend"); return nil }); err == nil {
			t.Fatal("expired reservation allowed write")
		}
		rows, err := r.ClaimExpired(f.ctx, "evict-a", 1, time.Second)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		failed := errors.New("fixed object deletion failed")
		if err := r.CompleteEviction(f.ctx, rows[0], func() error { return failed }); !errors.Is(err, failed) {
			t.Fatal("failed deletion released quota", err)
		}
		b := f.capture(t, "fixture-b", "b")
		if err := r.Reserve(f.ctx, b); !errors.Is(err, domain.ErrSnapshotQuota) {
			t.Fatal("pending eviction stopped counting", err)
		}
		if _, err := f.pool.Exec(f.ctx, `UPDATE elitea_runtime.rust_compiled_snapshots SET eviction_until=clock_timestamp()-interval '1 second'`); err != nil {
			t.Fatal(err)
		}
		recovered, err := r.ClaimExpired(f.ctx, "evict-b", 1, time.Second)
		if err != nil || len(recovered) != 1 || recovered[0].Epoch != rows[0].Epoch+1 {
			t.Fatal("eviction crash recovery", err)
		}
		if err := r.CompleteEviction(f.ctx, rows[0], func() error { t.Fatal("stale deleter touched objects"); return nil }); !errors.Is(err, domain.ErrSnapshotConflict) {
			t.Fatal("stale deletion accepted", err)
		}
		if err := r.CompleteEviction(f.ctx, recovered[0], func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := r.Reserve(f.ctx, b); err != nil {
			t.Fatal("completed eviction retained quota", err)
		}
	})
	t.Run("original_execution_recovery_survives_index_expiry", func(t *testing.T) {
		f.reset(t)
		r := f.repository(t, compiledPGQuota())
		c := f.ready(t, r, f.capture(t, "fixture-a", "a"))
		d, _ := c.Descriptor()
		job := domain.SnapshotContentSHA256([]byte("execute-a"))
		digest, _ := domain.SnapshotJobDigest("execute", d.Binding, c.Root)
		_, err := f.pool.Exec(f.ctx, `INSERT INTO elitea_runtime.sandbox_jobs(tenant_id,project_id,job_key,request_digest,phase,owner_id,lease_epoch,lease_until,runtime_id,compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json) VALUES($1,$2,$3,$4,'dispatched','fixture-executor',1,clock_timestamp()+interval '1 minute','original-execution-runtime','execute',$5,$6,$7)`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(job), snapshotBytes(digest), snapshotBytes(c.Key), snapshotBytes(d.Binding.BasePreparedRequestSHA256), c.DescriptorJSON)
		if err != nil {
			t.Fatal(err)
		}
		f.expire(t, c)
		rows, err := r.ClaimExpired(f.ctx, "evict-recovery", 1, time.Minute)
		if err != nil || len(rows) != 1 {
			t.Fatal(err)
		}
		if err := r.CompleteEviction(f.ctx, rows[0], func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Ready(f.ctx, c.Scope, c.Key); !errors.Is(err, domain.ErrSnapshotMiss) {
			t.Fatal("removed index remained readable", err)
		}
		for _, phase := range []string{"dispatched", "completed", "uncertain"} {
			set := "phase='" + phase + "'"
			if phase == "completed" {
				set += ",result_json='{}',failure_code=NULL"
			}
			if phase == "uncertain" {
				set += ",result_json=NULL,failure_code='fixture_uncertain'"
			}
			if _, err := f.pool.Exec(f.ctx, `UPDATE elitea_runtime.sandbox_jobs SET `+set+` WHERE job_key=$1`, snapshotBytes(job)); err != nil {
				t.Fatal(err)
			}
			original, found, err := r.OriginalExecution(f.ctx, c.Scope, c.Key, job, c.Root)
			if err != nil || !found || original.RuntimeID != "original-execution-runtime" || string(original.DescriptorJSON) != string(c.DescriptorJSON) {
				t.Fatal("recovery tried fresh lookup/admission", err)
			}
		}
		if _, found, err := r.OriginalExecution(f.ctx, c.Scope, c.Key, job, strings.Repeat("a", 64)); !found || !errors.Is(err, domain.ErrSnapshotConflict) {
			t.Fatal("recovery changed pinned root", err)
		}
	})
	t.Run("row_lock_holds_selected_read_through_concurrent_expiry", func(t *testing.T) {
		f.reset(t)
		r := f.repository(t, compiledPGQuota())
		c := f.ready(t, r, f.capture(t, "fixture-a", "a"))
		started := make(chan struct{})
		release := make(chan struct{})
		readDone := make(chan error, 1)
		go func() {
			readDone <- r.WithReady(f.ctx, c.Scope, c.Key, c.Root, func(domain.SnapshotCandidate) error {
				close(started)
				select {
				case <-release:
					return nil
				case <-f.ctx.Done():
					return f.ctx.Err()
				}
			})
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("selected read did not lock")
		}
		expiryDone := make(chan error, 1)
		go func() {
			_, err := f.pool.Exec(f.ctx, `UPDATE elitea_runtime.rust_compiled_snapshots SET expires_at=created_at+interval '1 microsecond' WHERE snapshot_key=$1`, snapshotBytes(c.Key))
			expiryDone <- err
		}()
		select {
		case err := <-expiryDone:
			t.Fatal("expiry crossed selected read lock", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		if err := <-readDone; err != nil {
			t.Fatal(err)
		}
		if err := <-expiryDone; err != nil {
			t.Fatal(err)
		}
		rows, err := r.ClaimExpired(f.ctx, "evict-lock", 1, time.Minute)
		if err != nil || len(rows) != 1 {
			t.Fatal("expiry not recoverable after read", err)
		}
	})
	t.Run("original_compile_retention_released_only_after_eviction", func(t *testing.T) {
		f.reset(t)
		r := f.repository(t, compiledPGQuota())
		c := f.ready(t, r, f.capture(t, "fixture-a", "a"))
		_, err := f.pool.Exec(f.ctx, `DELETE FROM elitea_runtime.sandbox_jobs WHERE job_key=$1`, snapshotBytes(c.CompilationJobKey))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23001" ||
			pgErr.ConstraintName != "rust_compiled_snapshots_tenant_id_project_id_compilation_j_fkey" {
			t.Fatal("ready artifact lost original receipt retention", err)
		}
		f.expire(t, c)
		rows, err := r.ClaimExpired(f.ctx, "evict-retention", 1, time.Minute)
		if err != nil || len(rows) != 1 {
			t.Fatal(err)
		}
		if err := r.CompleteEviction(f.ctx, rows[0], func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM elitea_runtime.sandbox_jobs WHERE job_key=$1`, snapshotBytes(c.CompilationJobKey)); err != nil {
			t.Fatal("completed eviction blocked original retention policy", err)
		}
	})
	f.reset(t)
	t.Log(fmt.Sprintf("exact agentstate head; bounded max_conns=%d; isolated fixture lifecycle assertions completed", f.pool.Config().MaxConns))
}
