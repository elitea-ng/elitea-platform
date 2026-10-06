package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNodeRecoveryPreFrontierClaimServiceKeepsExactVisitOrCheckpointInspection(t *testing.T) {
	for _, original := range []bool{true, false} {
		for _, spec := range []struct {
			name, desired, status string
			want                  executionapp.ClaimDisposition
			deny                  bool
		}{
			{"no visit before first receipt", "RUNNING", "", executionapp.ClaimRecoverAgentModelCheckpoint, false},
			{"A resumed lost ACK", "RUNNING", "RESUMED", executionapp.ClaimRecoverAgentModelCheckpoint, false},
			{"A resumed B pending before projection", "RUNNING", "RESUMED", executionapp.ClaimRecoverAgentModelCheckpoint, false},
			{"latest B suspended", "SUSPENDED", "SUSPENDED", executionapp.ClaimRecoverNodeVisit, false},
			{"latest B authorized", "SUSPENDED", "AUTHORIZED", executionapp.ClaimRecoverNodeVisit, false},
			{"latest B stopped", "RUNNING", "STOPPED", executionapp.ClaimRecoverNodeVisit, false},
			{"latest B cancelled cannot fall back to A", "RUNNING", "CANCELLED", "", true},
			{"latest B reconciled cannot fall back to A", "RUNNING", "RECONCILED", "", true},
			{"stored suspended with inconsistent RUNNING", "RUNNING", "SUSPENDED", "", true},
		} {
			prefix := "replacement/"
			if original {
				prefix = "original/"
			}
			t.Run(prefix+spec.name, func(t *testing.T) {
				request, e, token, observed, expires := nodePrefrontierClaimFixture(t, original, spec.desired, spec.status)
				store := &scriptedStore{scriptedExecutor: e}
				repo, err := newClaimsRepository(store, func() (string, error) { return "replacement-claim", nil }, func() (runtime.FenceToken, error) { return token, nil })
				if err != nil {
					t.Fatal(err)
				}
				service, err := executionapp.NewClaimService(repo, func() time.Time { return observed.Add(time.Hour) }, executionapp.MaxClaimLeaseTTLMillis.Duration())
				if err != nil {
					t.Fatal(err)
				}
				decision, err := service.Claim(t.Context(), request)
				if spec.deny {
					if !errors.Is(err, executionapp.ErrInvalidClaim) || decision.Disposition != "" {
						t.Fatal(decision, err)
					}
					for _, call := range e.execCalls {
						if strings.Contains(call.sql, "SET recovery_mode") {
							t.Fatal("denied visit changed mode")
						}
					}
					return
				}
				if err != nil || decision.Disposition != spec.want || decision.Lease.Fence.ExecutionID != request.ExecutionID || decision.Lease.Fence.Generation != request.Generation || !decision.LeaseObservedAt.Equal(observed) || !decision.Lease.ExpiresAt.Equal(expires) {
					t.Fatal(decision, err)
				}
				mode := "AGENT_MODEL_CHECKPOINT"
				if spec.want == executionapp.ClaimRecoverNodeVisit {
					mode = "NODE_RECOVERY"
					raw, _ := actualPrefrontierReceipt(t)
					if decision.NodeRecoveryReceipt != string(raw) {
						t.Fatal("stored receipt changed")
					}
				} else if decision.NodeRecoveryReceipt != "" {
					t.Fatal("inspection carried historical A receipt")
				}
				last := e.execCalls[len(e.execCalls)-1]
				if last.args[0] != mode || last.args[1] != decision.Lease.ClaimID || last.args[2] != request.ExecutionID || last.args[3] != int64(request.Generation) || last.args[8] != int64(decision.Lease.Fence.ClaimAttempt) || last.args[9] != int64(decision.Lease.Fence.LeaseEpoch) || !bytes.Equal(last.args[10].([]byte), decision.Lease.Fence.Token[:]) || strings.Contains(last.sql, "SET model_checkpoint_digest") {
					t.Fatal(last)
				}
				for _, call := range e.rowCalls {
					if strings.Contains(call.sql, "FROM elitea_runtime.node_recovery_visits") && (strings.Contains(call.sql, "status IN") || strings.Contains(call.sql, "AND status")) {
						t.Fatal("filtered receipt fallback", call)
					}
				}
				if original && decision.Lease.ClaimID != "original-claim" {
					t.Fatal("original claim replaced")
				}
				if !original && decision.Lease.Fence.ClaimAttempt != 2 {
					t.Fatal("replacement claim missing fence")
				}
			})
		}
	}
}

func nodePrefrontierClaimFixture(t *testing.T, original bool, desired, status string) (executionapp.ClaimRequest, *scriptedExecutor, runtime.FenceToken, time.Time, time.Time) {
	t.Helper()
	digest := runtime.SHA256([]byte("published-signed-envelope"))
	token := runtime.FenceToken(runtime.SHA256([]byte("prefrontier-token")))
	request := testClaimRequest(digest)
	request.ExecutionID = "0123456789abcdef0123456789abcdef"
	request.CapabilityID = executiondomain.AgentApplicationCapability
	request.NodeRecovery = true
	expires := time.Date(2030, 7, 16, 13, 0, 0, 0, time.UTC)
	observed := expires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration())
	visit := scriptedRow{err: pgx.ErrNoRows}
	if status != "" {
		raw, digest := actualPrefrontierReceipt(t)
		visit = scriptedRow{values: []any{raw, digest, status}}
	}
	rows := []scriptedRow{claimExecutionRow(desired, "RUNNING", true, digest[:], digest[:], true)}
	tags := []pgconn.CommandTag{}
	if original {
		observed = expires.Add(-10 * time.Second)
		rows = append(rows, scriptedRow{values: []any{"original-claim", request.CommandID, request.ExecutionID, int64(request.Generation), request.WorkloadIdentity, request.WorkloadSessionID, request.ProducerID, int64(1), int64(1), token[:], expires, desired, true, observed}}, scriptedRow{err: pgx.ErrNoRows}, scriptedRow{values: []any{int64(7)}}, visit)
	} else {
		rows = append(rows, scriptedRow{err: pgx.ErrNoRows})
		if desired == "SUSPENDED" {
			rows = append(rows, visit)
		}
		rows = append(rows, scriptedRow{values: []any{int64(2)}}, scriptedRow{values: []any{expires, observed, int64(7)}}, scriptedRow{err: pgx.ErrNoRows}, scriptedRow{values: []any{false}}, visit)
		tags = append(tags, pgconn.NewCommandTag("UPDATE 1"))
	}
	tags = append(tags, pgconn.NewCommandTag("UPDATE 1"))
	return request, &scriptedExecutor{rowResults: rows, execTags: tags}, token, observed, expires
}

func actualPrefrontierReceipt(t *testing.T) ([]byte, []byte) {
	t.Helper()
	raw, err := os.ReadFile("../../../../../../libs/jsonschema/runtime/v1/fixtures/node-recovery-actual-frontier-a-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != "d8842fdc746183d1ac18e7aa798e968c93f08361cb61a8afe2c9f29760a81440" {
		t.Fatal("actual Worker A receipt changed")
	}
	return raw, digest[:]
}

func TestNodeRecoveryPreFrontierInspectionDoesNotGrantFreshBeginOrInvoke(t *testing.T) {
	request, e, token, _, _ := nodePrefrontierClaimFixture(t, true, "RUNNING", "")
	repo, _ := newClaimsRepository(&scriptedStore{scriptedExecutor: e}, func() (string, error) { return "unused", nil }, func() (runtime.FenceToken, error) { return token, nil })
	service, _ := executionapp.NewClaimService(repo, time.Now, executionapp.MaxClaimLeaseTTLMillis.Duration())
	d, err := service.Claim(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	e.rowResults = append(e.rowResults, scriptedRow{values: []any{"RUNNING", "RUNNING", "MAY_HAVE_STARTED", true}}, scriptedRow{values: []any{"RUNNING", "RUNNING", "MAY_HAVE_STARTED", true}})
	n := len(e.execCalls)
	if got, err := service.BeginExecution(t.Context(), d.Lease.Fence); err != nil || got != executionapp.BeginExecutionAlreadyStarted {
		t.Fatal(got, err)
	}
	if got, err := service.AuthorizeInvocation(t.Context(), d.Lease.Fence); err != nil || got != executionapp.AuthorizeInvocationAlready {
		t.Fatal(got, err)
	}
	if len(e.execCalls) != n {
		t.Fatal("inspection granted fresh business authority")
	}
}

func TestNodeRecoveryPreFrontierModePromotionRechecksFullFenceAndKeepsDigest(t *testing.T) {
	request, e, token, _, _ := nodePrefrontierClaimFixture(t, true, "SUSPENDED", "SUSPENDED")
	repo, _ := newClaimsRepository(&scriptedStore{scriptedExecutor: e}, func() (string, error) { return "unused", nil }, func() (runtime.FenceToken, error) { return token, nil })
	service, _ := executionapp.NewClaimService(repo, time.Now, executionapp.MaxClaimLeaseTTLMillis.Duration())
	d, err := service.Claim(t.Context(), request)
	if err != nil || d.Disposition != executionapp.ClaimRecoverNodeVisit {
		t.Fatal(d, err)
	}
	q := e.execCalls[len(e.execCalls)-1]
	for _, required := range []string{"$1='NODE_RECOVERY' AND $12='SUSPENDED' AND c.recovery_mode='AGENT_MODEL_CHECKPOINT'", "c.claim_attempt=$9", "c.lease_epoch=$10", "c.fence_token=$11", "c.lease_expires_at>clock_timestamp()", "command.deadline>clock_timestamp()", "ws.revoked_at IS NULL"} {
		if !strings.Contains(q.sql, required) {
			t.Fatal(required)
		}
	}
	if strings.Contains(q.sql, "SET model_checkpoint_digest") || strings.Contains(q.sql, "SET invocation_state") || strings.Contains(q.sql, "SET desired_state") {
		t.Fatal("promotion reset durable authority")
	}
	e.execTags = []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")}
	if err := setNodeRecoveryClaimMode(context.Background(), e, d.Lease, "NODE_RECOVERY"); !errors.Is(err, runtime.ErrStaleFence) {
		t.Fatal("expired or stale promotion accepted", err)
	}
}

func TestNodeRecoveryPreFrontierPrivateControlRequiresNodeMode(t *testing.T) {
	if !strings.Contains(recoveryClaimAuthoritySQL, "c.recovery_mode='NODE_RECOVERY'") {
		t.Fatal("inspection claim can use historical node control")
	}
	claim := recoveryClaim()
	e := &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}}}
	s := &recoveryTxStore{scriptedExecutor: e}
	r := recoveryRepo(t, s)
	if _, err := r.PollNodeRecovery(t.Context(), claim); err == nil || len(e.execCalls) != 0 {
		t.Fatal("inspection control accepted", err)
	}
}

func TestNodeRecoveryPreFrontierNodeOnlyPreservesLiveCancellationAndDrainObservation(t *testing.T) {
	for _, desired := range []string{"CANCELLED", "DRAINING"} {
		t.Run(desired, func(t *testing.T) {
			request, e, token, observed, _ := nodePrefrontierClaimFixture(t, true, desired, "CANCELLED")
			e.rowResults = e.rowResults[:4]
			repo, _ := newClaimsRepository(&scriptedStore{scriptedExecutor: e}, func() (string, error) {
				t.Fatal("live cancelled claim replaced")
				return "", nil
			}, func() (runtime.FenceToken, error) { return token, nil })
			service, _ := executionapp.NewClaimService(repo, time.Now, executionapp.MaxClaimLeaseTTLMillis.Duration())
			d, err := service.Claim(t.Context(), request)
			if err != nil || d.Disposition != executionapp.ClaimRecoverRunningNoACK || string(d.Lease.DesiredState) != desired || d.Lease.ClaimID != "original-claim" || !d.LeaseObservedAt.Equal(observed) || len(e.execCalls) != 0 || d.NodeRecoveryReceipt != "" {
				t.Fatal("terminal control changed to node inspection", d, err)
			}
		})
	}
}

func TestNodeRecoveryPreFrontierServiceRejectsMixedFlagsAndClaimDigestMismatch(t *testing.T) {
	for _, name := range []string{"mixed flags", "foreign capability", "changed signed envelope"} {
		t.Run(name, func(t *testing.T) {
			request, e, token, _, _ := nodePrefrontierClaimFixture(t, true, "RUNNING", "")
			switch name {
			case "mixed flags":
				request.AgentModelCheckpointRecovery = true
			case "foreign capability":
				request.CapabilityID = executiondomain.ToolkitCallToolCapability
			case "changed signed envelope":
				request.SignedEnvelopeDigest = runtime.SHA256([]byte("different command"))
			}
			repo, _ := newClaimsRepository(&scriptedStore{scriptedExecutor: e}, func() (string, error) { t.Fatal("denied claim minted"); return "", nil }, func() (runtime.FenceToken, error) { return token, nil })
			service, _ := executionapp.NewClaimService(repo, time.Now, executionapp.MaxClaimLeaseTTLMillis.Duration())
			if _, err := service.Claim(t.Context(), request); !errors.Is(err, executionapp.ErrInvalidClaim) || len(e.execCalls) != 0 {
				t.Fatal(err, e.execCalls)
			}
		})
	}
}

func TestNodeRecoveryPreFrontierModelDigestRemainsOneUsePerClaim(t *testing.T) {
	request, e, token, _, _ := nodePrefrontierClaimFixture(t, true, "RUNNING", "RESUMED")
	repo, _ := newClaimsRepository(&scriptedStore{scriptedExecutor: e}, func() (string, error) { return "unused", nil }, func() (runtime.FenceToken, error) { return token, nil })
	service, _ := executionapp.NewClaimService(repo, time.Now, executionapp.MaxClaimLeaseTTLMillis.Duration())
	d, err := service.Claim(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	a := runtime.SHA256([]byte("current root checkpoint A"))
	b := runtime.SHA256([]byte("later root checkpoint B"))
	for _, digest := range []runtime.Digest{a, b} {
		e.rowResults = append(e.rowResults, scriptedRow{values: []any{"RUNNING", "RUNNING", "MAY_HAVE_STARTED", executiondomain.AgentApplicationCapability, "AGENT_MODEL_CHECKPOINT", a[:], true}})
		n := len(e.execCalls)
		got, err := service.AuthorizeAgentModelCheckpoint(t.Context(), d.Lease.Fence, digest)
		if digest == a {
			if err != nil || got != executionapp.AuthorizeInvocationAlready {
				t.Fatal(got, err)
			}
		} else if !errors.Is(err, executionapp.ErrInvalidClaim) {
			t.Fatal("same claim replaced checkpoint digest", got, err)
		}
		if len(e.execCalls) != n {
			t.Fatal("checkpoint digest replay wrote authority")
		}
	}
}
