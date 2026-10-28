package scale

// TestGovernedScale is the synthetic scale run of the governed credential
// profile (issue #1124 work item 4). It seeds the fixture and measures, on
// one PostgreSQL:
//
//   - admission: the #999 method (100-start bursts at 40 concurrency across
//     3 replicas), legacy and governed;
//   - redeem: the in-process credential broker of 3 replicas over a fake
//     secret backend, with the security audit relay draining alongside;
//   - refresh contention: N concurrent redemptions of one provider OAuth
//     grant inside its refresh skew, across 3 replicas;
//   - revocation to deny: an operator transition commits, and the next
//     redemption of the run on another replica must deny;
//   - the NATS invalidation fan-out, when ELITEA_TEST_NATS_URL is set.
//
// It runs only with ELITEA_GOVERNED_SCALE=1 (the governed-scale workflow).
// The report goes to stdout and, with ELITEA_GOVERNED_SCALE_REPORT, to that
// file.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/refresh"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/commandbus"
	"google.golang.org/protobuf/proto"
)

const (
	scaleEnv       = "ELITEA_GOVERNED_SCALE"
	scaleReportEnv = "ELITEA_GOVERNED_SCALE_REPORT"
	scaleNATSEnv   = "ELITEA_TEST_NATS_URL"
	// Dimension overrides.
	scaleTeamsEnv  = "ELITEA_GOVERNED_SCALE_TEAMS"
	scaleAppsEnv   = "ELITEA_GOVERNED_SCALE_APPS"
	scaleAgentsEnv = "ELITEA_GOVERNED_SCALE_AGENTS"
	scaleGrantsEnv = "ELITEA_GOVERNED_SCALE_GRANTS"
	// Load overrides.
	scaleRedeemsEnv = "ELITEA_GOVERNED_SCALE_REDEEMS"
)

// The #999 method: a 100-start burst at 40 concurrency across 3 replicas.
const (
	admissionReplicas    = 3
	admissionBurst       = 100
	admissionConcurrency = 40
	admissionBursts      = 5
	admissionCap         = 1024

	redeemReplicas    = 3
	redeemWorkers     = 32
	redeemRuns        = 1500
	redeemDefault     = 6000
	redeemWarmup      = 300
	redeemWarmupRuns  = 100
	redeemOneTeam     = 3000
	refreshCallers    = 60
	refreshRounds     = 5
	refreshDelay      = 40 * time.Millisecond
	revocationTrials  = 10
	natsChanges       = 500
	relayBacklogProbe = 100 * time.Millisecond
)

func intEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err, "%s", name)
	require.Positive(t, n, "%s", name)
	return n
}

func TestGovernedScale(t *testing.T) {
	if os.Getenv(scaleEnv) != "1" {
		t.Skipf("set %s=1 (and %s) to run the governed scale measurements", scaleEnv, scaleURLEnv)
	}
	config := newScaleDatabase(t)
	ctx := context.Background()
	report := &Report{}
	dims := Dimensions{
		Teams: intEnv(t, scaleTeamsEnv, DefaultTeams), AppsPerTeam: intEnv(t, scaleAppsEnv, DefaultAppsPerTeam),
		AgentsPerApp: intEnv(t, scaleAgentsEnv, DefaultAgentsPerApp), GrantsPerAgent: intEnv(t, scaleGrantsEnv, DefaultGrantsPerAgent),
	}.WithDefaults()

	admin := openPool(t, config, 24)
	var serverVersion, syncCommit, maxConnections string
	require.NoError(t, admin.QueryRow(ctx, `
SELECT current_setting('server_version'), current_setting('synchronous_commit'), current_setting('max_connections')`).
		Scan(&serverVersion, &syncCommit, &maxConnections))
	report.Info("env", "host", "goos", runtime.GOOS, "goarch", runtime.GOARCH, "cpus", runtime.NumCPU(),
		"go", runtime.Version(), "postgres", strings.Fields(serverVersion)[0],
		"synchronous_commit", syncCommit, "max_connections", maxConnections)
	report.Info("dimensions", "synthetic", "teams", dims.Teams, "calling_apps_per_team", dims.AppsPerTeam,
		"agents_per_app", dims.AgentsPerApp, "grants_per_agent", dims.GrantsPerAgent,
		"agents", dims.Agents(), "grants", dims.Agents()*dims.GrantsPerAgent)

	fixture, err := Seed(ctx, admin, dims)
	require.NoError(t, err)
	report.Info("seed", "fixture", "seconds", fmt.Sprintf("%.1f", fixture.Elapsed.Seconds()))
	for _, table := range CountedTables() {
		report.Info("rows", table, "count", fixture.Rows[table])
	}
	require.Equal(t, dims.Agents(), fixture.Rows["elitea_identity.agent_identity_bindings"])
	require.Equal(t, dims.Agents()*dims.GrantsPerAgent, fixture.Rows["elitea_identity.credential_use_grants"])
	require.NoError(t, analyze(ctx, admin))
	// The phases below stay inside PostgreSQL's default 100 connections.
	admin.Close()

	taken := map[int]bool{}
	t.Run("admission", func(t *testing.T) { measureAdmission(t, config, fixture, report) })
	t.Run("redeem", func(t *testing.T) { measureRedeem(t, config, fixture, report, taken) })
	t.Run("refresh", func(t *testing.T) { measureRefresh(t, config, fixture, report, taken) })
	t.Run("revocation", func(t *testing.T) { measureRevocation(t, config, fixture, report, taken) })
	t.Run("nats", func(t *testing.T) { measureNATS(t, fixture, report) })

	text := report.String()
	fmt.Print(text)
	if path := os.Getenv(scaleReportEnv); path != "" {
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	}
}

// analyze refreshes the planner statistics after the bulk seed, as
// autovacuum would before production traffic reaches the tables.
func analyze(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `ANALYZE`)
	return err
}

// ---- admission (#999 method) ------------------------------------------------

func admissionPolicy(credential profile.Admission) repos.AgentExecutionDispatchPolicy {
	return repos.AgentExecutionDispatchPolicy{
		CredentialProfile: credential, StreamName: "elitea:runtime:agent:commands", CapabilityVersion: "1",
		ResourceClass: "agent", IsolationClass: "project", Priority: 1, DeadlineTTL: time.Hour,
		LimitsRevision: "agent-limits-v1", MaxOutstanding: admissionCap,
	}
}

func admissionInput(generation string) *runtimev1.AgentExecutionInputV1 {
	steps := int32(12)
	return &runtimev1.AgentExecutionInputV1{
		SchemaRevision: "elitea.runtime.agent-execution-input.v1",
		Llm:            []byte(`{"kwargs":{"model":"model-1"}}`), ChatHistory: []byte(`[]`), UserInput: []byte(`"hello"`),
		Tools: []byte(`[]`), Application: []byte(`{}`), InternalTools: []byte(`[]`), StepsLimit: &steps,
		McpTokens: []byte(`{}`), IgnoredMcpServers: []byte(`[]`), UserDeclinedMcpServers: []byte(`[]`),
		HitlDecisions: []byte(`[]`), ExecutionGeneration: proto.String(generation), Meta: []byte(`{}`),
		Persona: "generic", ContextSettings: []byte(`{}`), InvokedSkills: []byte(`[]`), AppliedSkills: []byte(`[]`),
		AttachedSkills: []byte(`[]`), InputAttachments: []byte(`[]`), ParallelReconcile: []byte(`null`),
		ParallelTerminalErrors: []byte(`[]`), ExceptionHandlingEnabled: proto.Bool(false), DebugMode: proto.Bool(false),
	}
}

// agentAdmission is the durable start of one ad-hoc agent run of a, the
// shape PR #998's burst admitted (no chat turn rows).
func agentAdmission(t *testing.T, a *Agent, suffix string) agentexecutionapp.Admission {
	t.Helper()
	ids := []string{"bundle-" + suffix, "content-" + suffix, "execution-" + suffix, "command-" + suffix, "outbox-" + suffix}
	cursor := 0
	factory, err := agentexecutionapp.NewInputBundleFactory(agentexecutionapp.InputProfile{
		Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, func() (string, error) { id := ids[cursor]; cursor++; return id, nil })
	require.NoError(t, err)
	bundle, binding, err := factory.Build(context.Background(), admissionInput("gen-"+suffix), "stream-"+suffix, "message-"+suffix, "chat_predict")
	require.NoError(t, err)
	tenant := strconv.FormatInt(a.TenantProjectID, 10)
	actor := strconv.FormatInt(a.SubjectUserID, 10)
	now := time.Now().UTC()
	record := executiondomain.Admission{
		IdempotencyScope: "scale/" + tenant + "/" + actor, IdempotencyKey: "request-" + suffix,
		RequestDigest: runtimedomain.SHA256([]byte("request:" + suffix)), InputBundle: bundle,
		Job: executiondomain.Job{
			ID: ids[2], CommandID: ids[3], TenantID: "scale", ResourceProjectID: tenant, ProjectionProjectID: tenant,
			ActorID: actor, CapabilityID: executiondomain.AgentAdhocCapability, Generation: 1,
			State: executiondomain.JobPending, CreatedAt: now,
		},
		Outbox: executiondomain.OutboxRecord{ID: ids[4], CommandID: ids[3], ExecutionID: ids[2], Generation: 1, CreatedAt: now},
	}
	require.NoError(t, record.Validate())
	require.NoError(t, binding.Validate(record.InputBundle))
	return agentexecutionapp.Admission{Record: record, Binding: binding}
}

type admitter struct {
	repository *repos.AgentExecutionJobsRepository
	resolver   *configurationapp.GovernedReferenceResolver
}

// admit is one start. Governed, it resolves and freezes every toolkit's
// exact reference through the real resolver (#1119), then admits under the
// agent's mandate as an unattended governed entry point would (#1122): the
// transaction pins the run, binds the mandate, its binding and delegation,
// and writes the security context.
func (m admitter) admit(ctx context.Context, a *Agent, admission agentexecutionapp.Admission, governed bool) error {
	if governed {
		freeze := &agentexecutionapp.GovernedCredentialFreeze{AgentBindingID: a.BindingID}
		for _, tk := range a.Toolkits {
			resolution, err := m.resolver.Resolve(ctx, a.TenantProjectID, a.BindingID, tk.ID, tk.Digest,
				configurationapp.ToolkitCredentialRef{CredentialReferenceID: tk.CredentialReferenceID})
			if err != nil {
				return fmt.Errorf("resolve: %w", err)
			}
			freeze.References = append(freeze.References, agentexecutionapp.GovernedCredentialReference{
				ToolkitID: tk.ID, CredentialReferenceID: resolution.CredentialReferenceID,
				TargetBindingID: resolution.TargetBindingID, Generation: resolution.Generation, ToolkitDigest: tk.Digest,
			})
		}
		admission.GovernedCredentials = freeze
		ctx = securitycontext.WithRunAuthority(ctx, securitycontext.RunAuthority{
			MandateID: a.MandateID, SubjectUserID: a.SubjectUserID,
			Caller: securitycontext.Caller{AuthenticationSource: "api_key", CallingApplicationID: a.CallingApplicationID},
		})
	}
	outcome, err := m.repository.AdmitAgentExecution(ctx, admission)
	if err != nil {
		return err
	}
	if !outcome.Created {
		return errors.New("a fresh start was answered as a replay")
	}
	return nil
}

func measureAdmission(t *testing.T, config *pgxpool.Config, f *Fixture, report *Report) {
	ctx := context.Background()
	// Admission changes no fixture row an agent's later phases read, so it
	// samples without taking.
	agents := sample(f, admissionBurst*(admissionBursts+1)*2, map[int]bool{})
	settle := openPool(t, config, 2)
	for _, governed := range []bool{false, true} {
		name := "legacy"
		credential := profile.LegacyAdmission(false)
		if governed {
			name, credential = "governed", profile.GovernedAdmission(f.Revision)
		}
		admitters := make([]admitter, admissionReplicas)
		pools := make([]*pgxpool.Pool, admissionReplicas)
		for i := range admitters {
			pool := openPool(t, config, replicaPoolSize)
			pools[i] = pool
			repository, err := repos.NewAgentExecutionJobsRepository(pool, admissionPolicy(credential), 1)
			require.NoError(t, err)
			grants := repos.NewCredentialGrants(pool, nil)
			resolver, err := configurationapp.NewGovernedReferenceResolver(grants, grants, grants, nil)
			require.NoError(t, err)
			admitters[i] = admitter{repository: repository, resolver: resolver}
		}
		samples := &collector{}
		// Burst 0 warms the pools and the plans; it is not recorded.
		for burst := 0; burst <= admissionBursts; burst++ {
			_, err := settle.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = clock_timestamp() WHERE settled_at IS NULL`)
			require.NoError(t, err)
			starts := make(chan int, admissionBurst)
			for i := range admissionBurst {
				starts <- i
			}
			close(starts)
			var wg sync.WaitGroup
			errs := make(chan error, admissionBurst)
			for range admissionConcurrency {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range starts {
						offset := 0
						if governed {
							offset = len(agents) / 2
						}
						a := agents[offset+(burst*admissionBurst+i)%(len(agents)/2)]
						admission := agentAdmission(t, a, fmt.Sprintf("%s-%d-%d", name, burst, i))
						started := time.Now()
						err := admitters[i%admissionReplicas].admit(ctx, a, admission, governed)
						if err != nil {
							errs <- fmt.Errorf("%s start %d/%d: %w", name, burst, i, err)
							continue
						}
						if burst > 0 {
							samples.add(time.Since(started))
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}
		}
		report.Latency("admission."+name, Summarize(samples.samples),
			"bursts", admissionBursts, "burst", admissionBurst, "concurrency", admissionConcurrency, "replicas", admissionReplicas)
		for _, pool := range pools {
			pool.Close()
		}
	}
	// Every governed start left its pin and security context.
	var contexts int
	require.NoError(t, settle.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.execution_security_contexts c
JOIN elitea_runtime.execution_admission_profiles p USING (execution_id)
WHERE p.profile = 'elitea.credential-profile.governed.v1' AND c.execution_id LIKE 'execution-governed-%'`).Scan(&contexts))
	require.Equal(t, admissionBurst*(admissionBursts+1), contexts)
}

// ---- redeem and audit lag ---------------------------------------------------

func measureRedeem(t *testing.T, config *pgxpool.Config, f *Fixture, report *Report, taken map[int]bool) {
	ctx := context.Background()
	total := intEnv(t, scaleRedeemsEnv, redeemDefault)
	admin := openPool(t, config, 4)
	// The warm-up has runs of its own, so every measured run's first
	// redemption (the one that opens its agent session) is measured.
	agents := sample(f, min(redeemRuns+redeemWarmupRuns, len(f.Agents)/2), taken)
	require.Greater(t, len(agents), redeemWarmupRuns, "the fixture is too small for the redeem phase")
	warmupRuns := seedRuns(t, ctx, admin, f, agents[:redeemWarmupRuns], "redeem-warmup")
	runs := seedRuns(t, ctx, admin, f, agents[redeemWarmupRuns:], "redeem")
	secrets := &fakeSecrets{}
	replicas := make([]*replica, redeemReplicas)
	for i := range replicas {
		replicas[i] = newReplica(t, config, f, fmt.Sprintf("main-%d", i), secrets, replicaOptions{})
	}

	relayPool := openPool(t, config, 4)
	relay, err := securityaudit.NewRelay(relayPool, securityaudit.RelayConfig{
		BacklogAlertThreshold: f.Revision.Document.AuditBacklogAlertThreshold, Logger: quietLogger,
	})
	require.NoError(t, err)
	// The seed's own audit rows are relayed first, so the lag below is the
	// load's, not the seed backlog's. That drain is the relay's throughput.
	drainStart := time.Now()
	drained, err := relay.Drain(ctx)
	require.NoError(t, err)
	require.Zero(t, drained.Backlog)
	report.Info("throughput", "audit.relay_drain", "rows", drained.Stored,
		"rows_per_second", fmt.Sprintf("%.0f", float64(drained.Stored)/time.Since(drainStart).Seconds()))
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relay.Run(relayCtx) }()
	var maxBacklog atomic.Int64
	probeCtx, stopProbe := context.WithCancel(ctx)
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		ticker := time.NewTicker(relayBacklogProbe)
		defer ticker.Stop()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-ticker.C:
				if n, err := relay.Backlog(probeCtx); err == nil && n > maxBacklog.Load() {
					maxBacklog.Store(n)
				}
			}
		}
	}()

	loadStart := time.Now()
	spread := redeemLoad(t, ctx, replicas, warmupRuns, interleave(runs), total)
	report.Latency("redeem.all", Summarize(spread.all()), "replicas", redeemReplicas, "workers", redeemWorkers,
		"runs", len(runs), "teams", teamsOf(agents[redeemWarmupRuns:]), "per_second", spread.perSecond())
	report.Latency("redeem.first_of_run", Summarize(spread.first))
	report.Latency("redeem.repeat", Summarize(spread.later))
	// The same load on one team's agents: every audit append takes that
	// tenant's one chain head, so this is the per-team ceiling.
	team := teamAgents(f, taken)
	require.NotEmpty(t, team, "no untouched team is left for the one-team load")
	hot := redeemLoad(t, ctx, replicas, warmupRuns, seedRuns(t, ctx, admin, f, team, "redeem-team"), redeemOneTeam)
	report.Latency("redeem.one_team", Summarize(hot.all()), "replicas", redeemReplicas, "workers", redeemWorkers,
		"runs", len(team), "teams", 1, "per_second", hot.perSecond())
	require.EqualValues(t, total+redeemOneTeam+2*redeemWarmup, secrets.reads.Load())

	// Let the relay drain everything the load appended, then read each
	// row's time in the outbox.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		n, err := relay.Backlog(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
		require.True(t, time.Now().Before(deadline), "the audit backlog did not drain: %d rows", n)
		time.Sleep(50 * time.Millisecond)
	}
	stopProbe()
	<-probeDone
	stopRelay()
	<-relayDone
	rows, err := admin.Query(ctx, `
SELECT stored_at - created_at FROM elitea_runtime.security_audit_outbox WHERE created_at >= $1`, loadStart)
	require.NoError(t, err)
	lags, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Duration, error) {
		var lag time.Duration
		err := row.Scan(&lag)
		return lag, err
	})
	require.NoError(t, err)
	report.Latency("audit.outbox_lag", Summarize(lags), "relay_interval_ms", securityaudit.DefaultRelayInterval.Milliseconds(),
		"relay_batch", securityaudit.DefaultRelayBatchSize, "max_backlog", maxBacklog.Load())
	var unverified int
	require.NoError(t, admin.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox o
LEFT JOIN elitea_identity.security_audit_events e ON e.seq = o.seq AND e.hash = o.hash
WHERE e.seq IS NULL`).Scan(&unverified))
	require.Zero(t, unverified, "an outbox row is missing from the durable store")
}

// loadResult is one redeem load's measured latencies.
type loadResult struct {
	first, later []time.Duration
	elapsed      time.Duration
}

func (l loadResult) all() []time.Duration {
	return append(append([]time.Duration(nil), l.first...), l.later...)
}

func (l loadResult) perSecond() string {
	return fmt.Sprintf("%.0f", float64(len(l.first)+len(l.later))/l.elapsed.Seconds())
}

// redeemLoad runs redeemWarmup unrecorded redemptions of warmup, then total
// redemptions of runs round-robin, on redeemWorkers workers spread over the
// replicas. Every redemption must be permitted. A run's first redemption
// opens its agent session, so it is reported apart.
func redeemLoad(t *testing.T, ctx context.Context, replicas []*replica, warmup, runs []*run, total int) loadResult {
	t.Helper()
	var loadStart time.Time
	first, later := &collector{}, &collector{}
	work := make(chan int, total+redeemWarmup)
	for i := range total + redeemWarmup {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	var failures atomic.Int64
	var failure atomic.Value
	var started sync.Once
	for w := range redeemWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if i == redeemWarmup {
					started.Do(func() { loadStart = time.Now() })
				}
				r := warmup[i%len(warmup)]
				if i >= redeemWarmup {
					r = runs[(i-redeemWarmup)%len(runs)]
				}
				req := r.request()
				o := redeem(ctx, replicas[w%len(replicas)].broker, req)
				if !o.permitted {
					failures.Add(1)
					failure.Store(fmt.Sprintf("redeem %d: code=%s reason=%s err=%v", i, o.code, o.reason, o.err))
					continue
				}
				if i < redeemWarmup {
					continue
				}
				if req.Invocation.ToolCallID == "call-1" {
					first.add(o.end.Sub(o.start))
				} else {
					later.add(o.end.Sub(o.start))
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d redemptions were not permitted; last: %v", failures.Load(), failure.Load())
	}
	return loadResult{first: first.samples, later: later.samples, elapsed: time.Since(loadStart)}
}

// interleave orders runs so neighbours belong to different teams, as
// independent teams' traffic arrives: round-robin over the teams.
func interleave(runs []*run) []*run {
	byTeam := map[int64][]*run{}
	var teams []int64
	for _, r := range runs {
		if _, ok := byTeam[r.agent.TenantProjectID]; !ok {
			teams = append(teams, r.agent.TenantProjectID)
		}
		byTeam[r.agent.TenantProjectID] = append(byTeam[r.agent.TenantProjectID], r)
	}
	out := make([]*run, 0, len(runs))
	for len(out) < len(runs) {
		for _, team := range teams {
			if q := byTeam[team]; len(q) > 0 {
				out = append(out, q[0])
				byTeam[team] = q[1:]
			}
		}
	}
	return out
}

// teamAgents takes every agent of the highest-numbered team none of whose
// agents is taken yet.
func teamAgents(f *Fixture, taken map[int]bool) []*Agent {
	byTeam := map[int64][]int{}
	for i := range f.Agents {
		byTeam[f.Agents[i].TenantProjectID] = append(byTeam[f.Agents[i].TenantProjectID], i)
	}
	for team := int64(f.Dimensions.Teams) + 1; team >= 1; team-- {
		indexes := byTeam[team]
		free := len(indexes) > 0
		for _, i := range indexes {
			free = free && !taken[i]
		}
		if !free {
			continue
		}
		out := make([]*Agent, 0, len(indexes))
		for _, i := range indexes {
			taken[i] = true
			out = append(out, &f.Agents[i])
		}
		return out
	}
	return nil
}

// ---- refresh contention -----------------------------------------------------

// rotatingProvider is an Atlassian-like token endpoint: every refresh token
// is single use. A second refresh with an old token is invalid_grant.
type rotatingProvider struct {
	mu            sync.Mutex
	refreshToken  string
	generation    int
	challenges    map[string]string
	refreshCalls  atomic.Int32
	invalidGrants atomic.Int32
}

func (p *rotatingProvider) serve(w http.ResponseWriter, r *http.Request) {
	var fields map[string]string
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch fields["grant_type"] {
	case "refresh_token":
		p.refreshCalls.Add(1)
		time.Sleep(refreshDelay)
		p.mu.Lock()
		defer p.mu.Unlock()
		if fields["refresh_token"] != p.refreshToken {
			p.invalidGrants.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		p.generation++
		p.refreshToken = fmt.Sprintf("rt-%d", p.generation)
		_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":%q,"expires_in":3600,"token_type":"Bearer"}`, p.generation, p.refreshToken)
	case "authorization_code":
		p.mu.Lock()
		challenge := p.challenges[fields["code"]]
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(fields["code_verifier"]))
		if challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at-0","refresh_token":"rt-0","expires_in":3600,"scope":"read:jira-work offline_access","token_type":"Bearer"}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

const scaleRedirect = "http://127.0.0.1:9/api/v2/credential_consent/jira:callback"

// oauthToolkit adds an oauth_delegated Jira toolkit to a: its target,
// reference, toolkit binding and active grant.
func oauthToolkit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a *Agent) {
	t.Helper()
	tk := Toolkit{
		ID: int64(1_000_000_000 + a.Index), TargetBindingID: newID(), CredentialReferenceID: newID(), GrantID: newID(), Generation: 1,
		Settings: map[string]any{"jira_configuration": map[string]any{"elitea_title": "jira-oauth", "private": false}},
	}
	tk.Settings["jira_configuration"].(map[string]any)["credential_reference_id"] = tk.CredentialReferenceID
	var err error
	tk.Digest, err = credentialgrant.ToolkitDigest(ProviderType, tk.Settings)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity)
VALUES ($1::uuid, $2, 'jira', $3)`, tk.TargetBindingID, a.TenantProjectID, "https://oauth-"+strconv.Itoa(a.Index)+".jira.scale.test")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, $2, 'oauth_delegated', 'pgvault', 'scale/oauth', 'jira', $3::uuid, 1, 'active')`,
		tk.CredentialReferenceID, a.TenantProjectID, tk.TargetBindingID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO elitea_identity.agent_toolkit_bindings (agent_binding_id, toolkit_id, tenant_project_id, toolkit_digest)
VALUES ($1::uuid, $2, $3, $4)`, a.BindingID, tk.ID, a.TenantProjectID, tk.Digest[:])
	require.NoError(t, err)
	_, err = repos.NewCredentialGrants(pool, mustAppender(t)).CreateGrant(ctx, credentialgrant.CredentialUseGrant{
		ID: tk.GrantID, TenantProjectID: a.TenantProjectID, AgentBindingID: a.BindingID, ToolkitID: tk.ID,
		CredentialReferenceID: tk.CredentialReferenceID, TargetBindingID: tk.TargetBindingID,
		Capabilities: []credentialgrant.Capability{credentialgrant.CapUse}, OperationScopes: []string{Operation},
		Obligations: []string{credentialgrant.ObligationAuditSync}, State: credentialgrant.GrantActive,
		ProposedBy: ProposerID, ApprovedBy: ApproverID, LastTransitionBy: ApproverID,
	})
	require.NoError(t, err)
	a.Toolkits = []Toolkit{tk}
}

func mustAppender(t *testing.T) *securityaudit.PostgresAppender {
	t.Helper()
	audit, err := securityaudit.NewPostgresAppender("elitea-main/scale")
	require.NoError(t, err)
	return audit
}

func measureRefresh(t *testing.T, config *pgxpool.Config, f *Fixture, report *Report, taken map[int]bool) {
	ctx := context.Background()
	admin := openPool(t, config, 4)
	agent := sample(f, 1, taken)[0]
	copied := *agent
	oauthToolkit(t, ctx, admin, &copied)

	provider := &rotatingProvider{refreshToken: "rt-0", challenges: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(provider.serve))
	t.Cleanup(server.Close)
	client, err := provideroauth.NewClient(provideroauth.ProviderConfig{
		Provider: "jira", Flavor: provideroauth.FlavorAtlassian, ClientID: "client", ClientSecret: "client-secret",
		AuthorizeURL: server.URL + "/authorize", TokenURL: server.URL + "/oauth/token", RedirectURI: scaleRedirect,
		Scopes: []string{"read:jira-work", "offline_access"}, Timeout: 5 * time.Second,
	}, nil)
	require.NoError(t, err)

	// The user's consent, through the production flow.
	audit := mustAppender(t)
	grants, err := provideroauth.NewGrants(admin, MasterKey(), audit)
	require.NoError(t, err)
	consent, err := provideroauth.NewConsent(provideroauth.ConsentConfig{
		Grants: grants, Clients: []*provideroauth.Client{client},
		Targets: repos.NewCredentialGrants(admin, audit), Bindings: repos.NewAgentIdentityBindings(admin, audit),
	})
	require.NoError(t, err)
	tk := copied.Toolkits[0]
	started, err := consent.Start(ctx, provideroauth.StartRequest{
		UserID: copied.SubjectUserID, Provider: "jira", TenantProjectID: copied.TenantProjectID,
		AgentBindingID: copied.BindingID, TargetBindingID: tk.TargetBindingID,
	})
	require.NoError(t, err)
	authorization, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	provider.mu.Lock()
	provider.challenges["code-1"] = authorization.Query().Get("code_challenge")
	provider.mu.Unlock()
	_, err = consent.Callback(ctx, provideroauth.CallbackRequest{Provider: "jira", State: authorization.Query().Get("state"), Code: "code-1"},
		func(context.Context, int64) (int64, error) { return copied.SubjectUserID, nil })
	require.NoError(t, err)

	secrets := &fakeSecrets{}
	replicas := make([]*replica, 3)
	for i := range replicas {
		pool := openPool(t, config, 4)
		leases, err := refresh.NewLeases(pool)
		require.NoError(t, err)
		coordinator, err := refresh.NewCoordinator(leases, refresh.Config{Holder: fmt.Sprintf("main-%d", i)})
		require.NoError(t, err)
		source, err := provideroauth.NewTokenSource(provideroauth.TokenSourceConfig{
			Grants: mustGrants(t, pool), Leases: leases, Coordinator: coordinator, Clients: []*provideroauth.Client{client},
		})
		require.NoError(t, err)
		replicas[i] = newReplica(t, config, f, fmt.Sprintf("main-%d", i), secrets, replicaOptions{tokens: source})
	}
	r := seedRuns(t, ctx, admin, f, []*Agent{&copied}, "refresh")[0]
	// One redemption opens the run's agent session, so every round below
	// measures the refresh, not the first-use session open.
	require.True(t, redeem(ctx, replicas[0].broker, r.request()).permitted)

	samples := &collector{}
	for round := 1; round <= refreshRounds; round++ {
		_, err := admin.Exec(ctx, `
UPDATE elitea_identity.provider_oauth_grants SET access_token_expires_at = clock_timestamp() + interval '10 seconds'`)
		require.NoError(t, err)
		gate := make(chan struct{})
		outcomes := make([]outcome, refreshCallers)
		var wg sync.WaitGroup
		for i := range refreshCallers {
			wg.Add(1)
			req := r.request()
			go func() {
				defer wg.Done()
				<-gate
				outcomes[i] = redeem(ctx, replicas[i%len(replicas)].broker, req)
			}()
		}
		close(gate)
		wg.Wait()
		for i, o := range outcomes {
			require.True(t, o.permitted, "round %d caller %d: code=%s reason=%s err=%v", round, i, o.code, o.reason, o.err)
			samples.add(o.end.Sub(o.start))
		}
		require.EqualValues(t, round, provider.refreshCalls.Load(), "round %d: provider refresh calls", round)
	}
	require.Zero(t, provider.invalidGrants.Load(), "a rotated refresh token was used twice")
	report.Latency("refresh.contended_redeem", Summarize(samples.samples), "callers", refreshCallers, "rounds", refreshRounds,
		"replicas", len(replicas), "provider_refresh_calls", provider.refreshCalls.Load(), "invalid_grant", provider.invalidGrants.Load(),
		"provider_delay_ms", refreshDelay.Milliseconds())
}

func mustGrants(t *testing.T, pool *pgxpool.Pool) *provideroauth.Grants {
	t.Helper()
	g, err := provideroauth.NewGrants(pool, MasterKey(), mustAppender(t))
	require.NoError(t, err)
	return g
}

// ---- revocation to deny -----------------------------------------------------

type revocation struct {
	kind   string
	revoke func(ctx context.Context, writer *replica, a *Agent) error
}

func revocations() []revocation {
	return []revocation{
		{"grant", func(ctx context.Context, w *replica, a *Agent) error {
			for _, tk := range a.Toolkits {
				if _, err := w.grants.TransitionGrant(ctx, a.TenantProjectID, tk.GrantID, func(g credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
					g.State, g.Epoch, g.LastTransitionBy, g.UpdatedAt = credentialgrant.GrantRevoked, g.Epoch+1, ApproverID, time.Now().UTC()
					return g, nil
				}); err != nil {
					return err
				}
			}
			return nil
		}},
		{"mandate", func(ctx context.Context, w *replica, a *Agent) error {
			_, err := w.mandates.TransitionMandate(ctx, a.MandateID, func(m mandate.Mandate, _ agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
				m.State, m.RevocationEpoch, m.LastTransitionBy, m.UpdatedAt = mandate.StateRevoked, m.RevocationEpoch+1, ApproverID, time.Now().UTC()
				return m, nil
			})
			return err
		}},
		{"binding", func(ctx context.Context, w *replica, a *Agent) error {
			_, err := w.bindings.TransitionBinding(ctx, a.BindingID, func(b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
				b.State, b.RevocationEpoch, b.LastTransitionBy, b.UpdatedAt = agentidentity.BindingSuspended, b.RevocationEpoch+1, ApproverID, time.Now().UTC()
				return b, nil
			})
			return err
		}},
		{"delegation", func(ctx context.Context, w *replica, a *Agent) error {
			_, err := w.custody.Revoke(ctx, a.DelegationID, ApproverID)
			return err
		}},
		{"credential_reference", func(ctx context.Context, w *replica, a *Agent) error {
			// No production writer retires a reference yet; the epoch reader
			// treats a retired one as revoked.
			for _, tk := range a.Toolkits {
				if _, err := w.pool.Exec(ctx, `
UPDATE elitea_identity.credential_references SET state = 'retired', updated_at = clock_timestamp() WHERE id = $1::uuid`,
					tk.CredentialReferenceID); err != nil {
					return err
				}
			}
			return nil
		}},
	}
}

func measureRevocation(t *testing.T, config *pgxpool.Config, f *Fixture, report *Report, taken map[int]bool) {
	ctx := context.Background()
	admin := openPool(t, config, 4)
	secrets := &fakeSecrets{}
	writer := newReplica(t, config, f, "main-0", secrets, replicaOptions{})
	reader := newReplica(t, config, f, "main-1", secrets, replicaOptions{})
	all := &collector{}
	for _, rv := range revocations() {
		agents := sample(f, revocationTrials, taken)
		runs := seedRuns(t, ctx, admin, f, agents, "revoke-"+rv.kind)
		samples := &collector{}
		for trial, r := range runs {
			latency, violations := revokeAndWatch(t, ctx, writer, reader, rv, r)
			require.Zero(t, violations, "%s trial %d: a redemption that started after the commit was permitted", rv.kind, trial)
			samples.add(latency)
			all.add(latency)
		}
		report.Latency("revocation_to_deny."+rv.kind, Summarize(samples.samples), "trials", len(runs))
	}
	report.Latency("revocation_to_deny.all", Summarize(all.samples))
}

// revokeAndWatch keeps one redemption of r in flight on reader, commits the
// revocation on writer, and answers the time from the commit's return to
// the end of the first denied redemption that started after it, and how
// many redemptions that started after it were permitted (must be none).
func revokeAndWatch(t *testing.T, ctx context.Context, writer, reader *replica, rv revocation, r *run) (time.Duration, int) {
	t.Helper()
	var committed atomic.Int64
	results := make(chan outcome, 1024)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			results <- redeem(ctx, reader.broker, r.request())
		}
	}()
	defer func() { close(stop); <-done }()
	permits := 0
	for permits < 3 {
		o := <-results
		require.True(t, o.permitted, "%s: before the revocation: code=%s reason=%s err=%v", rv.kind, o.code, o.reason, o.err)
		permits++
	}
	require.NoError(t, rv.revoke(ctx, writer, r.agent))
	at := time.Now()
	committed.Store(at.UnixNano())
	violations := 0
	timeout := time.After(30 * time.Second)
	for {
		select {
		case o := <-results:
			if o.start.Before(at) {
				continue
			}
			if o.permitted {
				violations++
				continue
			}
			require.NoError(t, o.err)
			go func() {
				for range results {
				}
			}()
			return o.end.Sub(at), violations
		case <-timeout:
			t.Fatalf("%s: no redemption denied within 30s of the commit", rv.kind)
		}
	}
}

// ---- NATS invalidation fan-out ----------------------------------------------

// natsTransport carries the hub over core NATS on the production subject,
// as runtimecomposition's transport does.
type natsTransport struct{ conn *nats.Conn }

func (n natsTransport) Publish(data []byte) error {
	return n.conn.Publish(commandbus.SecurityInvalidationSubject, data)
}

func (n natsTransport) Subscribe(deliver func([]byte)) (func() error, error) {
	sub, err := n.conn.Subscribe(commandbus.SecurityInvalidationSubject, func(msg *nats.Msg) { deliver(msg.Data) })
	if err != nil {
		return nil, err
	}
	return sub.Unsubscribe, nil
}

func measureNATS(t *testing.T, f *Fixture, report *Report) {
	address := os.Getenv(scaleNATSEnv)
	if address == "" {
		report.Info("latency", "invalidation.nats", "measured", "no", "reason", scaleNATSEnv+"_unset")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hubs := make([]*epoch.Hub, 2)
	for i := range hubs {
		conn, err := nats.Connect(address, nats.Name(fmt.Sprintf("scale-main-%d", i)))
		require.NoError(t, err)
		t.Cleanup(conn.Close)
		hubs[i], err = epoch.NewHub(epoch.HubOptions{Logger: quietLogger})
		require.NoError(t, err)
		go func() { _ = hubs[i].Run(ctx, natsTransport{conn: conn}) }()
	}
	received := make(chan epoch.Change, natsChanges)
	hubs[1].Listen(func(c epoch.Change) { received <- c })
	// The subscription is up once a probe crosses.
	probe := epoch.SecurityEpoch{Kind: epoch.KindGrant, ID: f.Agents[0].Toolkits[0].GrantID, Value: 1}
	ready := time.After(10 * time.Second)
	for crossed := false; !crossed; {
		hubs[0].Notify(ctx, epoch.Change{Epoch: probe})
		select {
		case <-received:
			crossed = true
		case <-time.After(50 * time.Millisecond):
		case <-ready:
			t.Fatal("the NATS invalidation subscription did not come up")
		}
	}
	for len(received) > 0 {
		<-received
	}
	samples := &collector{}
	for i := range natsChanges {
		id := uuid.Must(uuid.NewV7()).String()
		sent := time.Now()
		hubs[0].Notify(ctx, epoch.Change{Epoch: epoch.SecurityEpoch{Kind: epoch.KindMandate, ID: id, Value: int64(i + 1)}, ChangedAt: sent})
		select {
		case c := <-received:
			require.Equal(t, id, c.Epoch.ID)
			samples.add(time.Since(sent))
		case <-time.After(5 * time.Second):
			t.Fatalf("change %d did not cross NATS", i)
		}
	}
	report.Latency("invalidation.nats", Summarize(samples.samples), "measured", "yes", "changes", natsChanges, "replicas", 2)
}
