package control

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

func TestNewServerRejectsMalformedCapabilityConfig(t *testing.T) {
	t.Parallel()
	manifest := validManifest()
	for name, mutate := range map[string]func(*ServerConfig){
		"bad required worker cap": func(cfg *ServerConfig) { cfg.RequiredWorkerCapabilities = []string{"BadCap"} },
		"dup required worker cap": func(cfg *ServerConfig) {
			cfg.RequiredWorkerCapabilities = []string{runtimedomain.CapabilityIdleCompaction, runtimedomain.CapabilityIdleCompaction}
		},
		"bad extra server cap": func(cfg *ServerConfig) { cfg.ExtraServerCapabilities = []string{"single"} },
		"extra server caps exceed built-in headroom": func(cfg *ServerConfig) {
			extra := make([]string, runtimedomain.MaxNegotiatedCapabilities-len(builtInServerCapabilities)+1)
			for i := range extra {
				extra[i] = fmt.Sprintf("runtime.extra_%02d.v1", i)
			}
			cfg.ExtraServerCapabilities = extra
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testControlServerConfig()
			mutate(&cfg)
			calls := []string{}
			if _, err := NewServer(
				cfg,
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: validLease()},
				inputResolverStub{calls: &calls, manifest: manifest},
				&settlementControllerStub{calls: &calls},
			); err == nil {
				t.Fatalf("%s: expected startup error", name)
			}
		})
	}

	// Exact headroom boundary (64 - len(builtInServerCapabilities) = 60 unique extras) succeeds.
	cfg := testControlServerConfig()
	exactExtras := make([]string, runtimedomain.MaxNegotiatedCapabilities-len(builtInServerCapabilities))
	for i := range exactExtras {
		exactExtras[i] = fmt.Sprintf("runtime.extra_%02d.v1", i)
	}
	cfg.ExtraServerCapabilities = exactExtras
	calls := []string{}
	if _, err := NewServer(
		cfg,
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: validLease()},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	); err != nil {
		t.Fatalf("exact headroom extras failed startup: %v", err)
	}
}

// Matrix covering Old-Worker (legacy booleans only), New-Worker
// (worker_capabilities only), and Dual-Worker (both set) against New-Main.
func TestClaimCommandTwoWayCapabilityNegotiationMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		mutateReq         func(*runtimev1.ClaimCommandRequestV1)
		wantModelRecovery bool
		wantNodeRecovery  bool
		wantFanoutDec     bool
		wantFanoutCreds   bool
		wantIdleCompact   bool
	}{
		{
			name: "old worker legacy booleans only",
			mutateReq: func(req *runtimev1.ClaimCommandRequestV1) {
				req.FanoutInterruptDecisions = true
				req.FanoutDelegatedCredentials = true
				req.IdleCompaction = true
				req.AgentModelCheckpointRecovery = true
			},
			wantModelRecovery: true,
			wantFanoutDec:     true,
			wantFanoutCreds:   true,
			wantIdleCompact:   true,
		},
		{
			name: "new worker capabilities list only",
			mutateReq: func(req *runtimev1.ClaimCommandRequestV1) {
				req.WorkerCapabilities = []string{
					runtimedomain.CapabilityFanoutInterruptDecisions,
					runtimedomain.CapabilityFanoutDelegatedCredentials,
					runtimedomain.CapabilityIdleCompaction,
					runtimedomain.CapabilityNodeRecovery,
					"runtime.future_feature.v2",
				}
			},
			wantNodeRecovery: true,
			wantFanoutDec:    true,
			wantFanoutCreds:  true,
			wantIdleCompact:  true,
		},
		{
			name: "dual worker legacy booleans plus capabilities",
			mutateReq: func(req *runtimev1.ClaimCommandRequestV1) {
				req.FanoutInterruptDecisions = true
				req.IdleCompaction = true
				req.WorkerCapabilities = []string{
					runtimedomain.CapabilityFanoutInterruptDecisions,
					runtimedomain.CapabilityFanoutDelegatedCredentials,
					runtimedomain.CapabilityIdleCompaction,
				}
			},
			wantFanoutDec:   true,
			wantFanoutCreds: true,
			wantIdleCompact: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			manifest := validAgentManifest()
			request, cmd := claimRequestForAgentManifest(t, manifest)
			tc.mutateReq(request)

			lease := validLease()
			lease.CapabilityID = executiondomain.AgentApplicationCapability
			calls := []string{}
			var captured executionapp.ClaimRequest
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				staticCommandVerifier{command: cmd, calls: &calls},
				claimControllerStub{calls: &calls, lease: lease, claimRequest: &captured},
				inputResolverStub{calls: &calls, manifest: manifest},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), request)
			if err != nil || response.GetRejection() != nil {
				t.Fatalf("claim failed: err=%v rejection=%v", err, response.GetRejection())
			}
			if captured.AgentModelCheckpointRecovery != tc.wantModelRecovery ||
				captured.NodeRecovery != tc.wantNodeRecovery ||
				captured.FanoutInterruptDecisions != tc.wantFanoutDec ||
				captured.FanoutDelegatedCredentials != tc.wantFanoutCreds ||
				captured.IdleCompaction != tc.wantIdleCompact {
				t.Fatalf("captured flags = %+v", captured)
			}
			serverCaps := response.GetReceipt().GetServerCapabilities()
			if !slices.Contains(serverCaps, runtimedomain.CapabilityNegotiation) ||
				!slices.Contains(serverCaps, runtimedomain.CapabilityPausedWriter) ||
				slices.Contains(serverCaps, runtimedomain.CapabilityToolkitTypedRefusals) {
				t.Fatalf("unexpected agent server_capabilities: %v", serverCaps)
			}
		})
	}
}

func TestClaimCommandRejectsMalformedAndOversizedWorkerCapabilities(t *testing.T) {
	t.Parallel()

	overCount := make([]string, runtimedomain.MaxNegotiatedCapabilities+1)
	for i := range overCount {
		overCount[i] = fmt.Sprintf("runtime.cap_%02d.v1", i)
	}
	overLen := "runtime." + strings.Repeat("x", runtimedomain.MaxCapabilityNameBytes)

	badCases := map[string][]string{
		"over max count": overCount,
		"over max bytes": {overLen},
		"empty entry":    {""},
		"uppercase":      {"runtime.BadCap.v1"},
		"single segment": {"noperiod"},
		"duplicate":      {runtimedomain.CapabilityIdleCompaction, runtimedomain.CapabilityIdleCompaction},
		"path escape":    {"runtime/../escape.v1"},
	}
	for name, caps := range badCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manifest := validManifest()
			request := claimRequestForManifest(t, manifest)
			request.WorkerCapabilities = caps

			calls := []string{}
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: validLease()},
				inputResolverStub{calls: &calls, manifest: manifest},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), request)
			if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION || response.GetRejection().GetRetryable() {
				t.Fatalf("%s: code=%v retryable=%v", name, code, response.GetRejection().GetRetryable())
			}
			if len(calls) != 0 {
				t.Fatalf("%s: malformed capabilities reached auth/claim: %v", name, calls)
			}
		})
	}
}

func TestClaimCommandGovernedAndRequiredCapabilitiesFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("governed accepted via worker_capabilities without legacy bool", func(t *testing.T) {
		t.Parallel()
		calls := []string{}
		req := governedClaimRequest(t, nil, false)
		req.WorkerCapabilities = []string{runtimedomain.CapabilityGovernedSecurityContext}
		resp, err := governedTestServer(t, &calls, nil).ClaimCommand(context.Background(), req)
		if code := rejectionCode(t, resp, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
			t.Fatalf("code=%v calls=%v", code, calls)
		}
		if !slices.Contains(resp.GetReceipt().GetServerCapabilities(), runtimedomain.CapabilityGovernedSecurityContext) {
			t.Fatalf("governed receipt missing server capability: %v", resp.GetReceipt().GetServerCapabilities())
		}
	})

	t.Run("missing required worker capability refuses before claim", func(t *testing.T) {
		t.Parallel()
		manifest := validManifest()
		cfg := testControlServerConfig()
		cfg.RequiredWorkerCapabilities = []string{"runtime.strict_contract.v1"}
		calls := []string{}
		server, err := NewServer(
			cfg,
			workloadAuthorizerStub{calls: &calls},
			verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
			claimControllerStub{calls: &calls, lease: validLease()},
			inputResolverStub{calls: &calls, manifest: manifest},
			&settlementControllerStub{calls: &calls},
		)
		if err != nil {
			t.Fatal(err)
		}
		// Old worker without required capability fails closed.
		req := claimRequestForManifest(t, manifest)
		resp, err := server.ClaimCommand(context.Background(), req)
		if code := rejectionCode(t, resp, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY || resp.GetRejection().GetRetryable() || claimed(calls) {
			t.Fatalf("missing required cap: code=%v retryable=%v calls=%v", code, resp.GetRejection().GetRetryable(), calls)
		}
		// New worker advertising required capability succeeds.
		calls = calls[:0]
		req.WorkerCapabilities = []string{"runtime.strict_contract.v1"}
		resp, err = server.ClaimCommand(context.Background(), req)
		if code := rejectionCode(t, resp, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
			t.Fatalf("present required cap: code=%v calls=%v", code, calls)
		}
	})

	t.Run("required legacy capability satisfied by old worker boolean", func(t *testing.T) {
		t.Parallel()
		manifest := validManifest()
		cfg := testControlServerConfig()
		cfg.RequiredWorkerCapabilities = []string{runtimedomain.CapabilityIdleCompaction}
		calls := []string{}
		server, err := NewServer(
			cfg,
			workloadAuthorizerStub{calls: &calls},
			verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
			claimControllerStub{calls: &calls, lease: validLease()},
			inputResolverStub{calls: &calls, manifest: manifest},
			&settlementControllerStub{calls: &calls},
		)
		if err != nil {
			t.Fatal(err)
		}
		req := claimRequestForManifest(t, manifest)
		req.IdleCompaction = true
		resp, err := server.ClaimCommand(context.Background(), req)
		if code := rejectionCode(t, resp, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
			t.Fatalf("legacy bool should satisfy required cap: code=%v calls=%v", code, calls)
		}
	})

	t.Run("conflicting node and checkpoint recovery across bool and capabilities fails closed", func(t *testing.T) {
		t.Parallel()
		manifest := validManifest()
		calls := []string{}
		claimSvc, err := executionapp.NewClaimService(
			claimRepoFunc(func(context.Context, executionapp.ClaimRequest, executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error) {
				t.Fatal("repository must not be called for conflicting recovery modes")
				return executionapp.ClaimDecision{}, nil
			}),
			nil,
			executionapp.MaxClaimLeaseTTLMillis.Duration(),
		)
		if err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(
			testControlServerConfig(),
			workloadAuthorizerStub{calls: &calls},
			verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
			claimSvc,
			inputResolverStub{calls: &calls, manifest: manifest},
			&settlementControllerStub{calls: &calls},
		)
		if err != nil {
			t.Fatal(err)
		}
		req := claimRequestForManifest(t, manifest)
		req.NodeRecovery = true
		req.WorkerCapabilities = []string{runtimedomain.CapabilityAgentModelCheckpointRecovery}
		resp, err := server.ClaimCommand(context.Background(), req)
		if code := rejectionCode(t, resp, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
			t.Fatalf("conflicting recovery modes: code=%v", code)
		}
	})
}

func TestClaimReceiptServerCapabilitiesForToolRunAndNonExecutableDispositions(t *testing.T) {
	t.Parallel()

	// Tool run advertises BOTH legacy toolkit_typed_refusals=true and
	// server_capabilities containing runtime.toolkit_typed_refusals.v1.
	manifest := validToolRunManifest()
	req, cmd := claimRequestForToolRunManifest(t, manifest)

	lease := validLease()
	lease.CapabilityID = executiondomain.ToolkitCallToolCapability
	calls := []string{}
	cfg := testControlServerConfig()
	cfg.ExtraServerCapabilities = []string{"runtime.extra_feature.v1"}
	server, err := NewServer(
		cfg,
		workloadAuthorizerStub{calls: &calls},
		staticCommandVerifier{command: cmd, calls: &calls},
		claimControllerStub{calls: &calls, lease: lease},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.ClaimCommand(context.Background(), req)
	if err != nil || resp.GetRejection() != nil {
		t.Fatalf("tool claim failed: err=%v rej=%v", err, resp.GetRejection())
	}
	if !resp.GetReceipt().GetToolkitTypedRefusals() {
		t.Fatal("legacy toolkit_typed_refusals must stay true on tool runs")
	}
	wantCaps := []string{
		runtimedomain.CapabilityNegotiation,
		"runtime.extra_feature.v1",
		runtimedomain.CapabilityToolkitTypedRefusals,
	}
	slices.Sort(wantCaps)
	if !slices.Equal(resp.GetReceipt().GetServerCapabilities(), wantCaps) {
		t.Fatalf("server_capabilities = %v, want %v", resp.GetReceipt().GetServerCapabilities(), wantCaps)
	}

	// Non-executable disposition omits server_capabilities.
	nonExecServer, err := NewServer(
		cfg,
		workloadAuthorizerStub{calls: &calls},
		staticCommandVerifier{command: cmd, calls: &calls},
		claimControllerStub{calls: &calls, lease: lease, disposition: executionapp.ClaimActiveLeaseNoACK},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = nonExecServer.ClaimCommand(context.Background(), req)
	if err != nil || resp.GetRejection() != nil {
		t.Fatalf("non-exec claim failed: err=%v rej=%v", err, resp.GetRejection())
	}
	if len(resp.GetReceipt().GetServerCapabilities()) != 0 {
		t.Fatalf("non-executable receipt carried server_capabilities: %v", resp.GetReceipt().GetServerCapabilities())
	}
}

type staticCommandVerifier struct {
	command *runtimev1.WorkerCommandV1
	calls   *[]string
}

func (v staticCommandVerifier) Verify(_ context.Context, _ *runtimev1.SignedWorkerCommandEnvelopeV1) (*runtimev1.WorkerCommandV1, error) {
	if v.calls != nil {
		*v.calls = append(*v.calls, "verify-command")
	}
	return proto.Clone(v.command).(*runtimev1.WorkerCommandV1), nil
}

func validAgentManifest() *runtimev1.ExecutionInputBundleV1 {
	return &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    "bundle-agent-1",
		ImmutableVersion: "v1",
		Entries: []*runtimev1.ExecutionInputEntryV1{
			{
				EntryId:          "agent-request",
				ImmutableVersion: "revision-1",
				SemanticRole:     executiondomain.AgentExecutionRequestRole,
				Content: &runtimev1.ScopedContentReferenceV1{
					ContentId:             "content-agent-1",
					ImmutableVersion:      "revision-1",
					MediaType:             executiondomain.AgentExecutionInputMediaType,
					ByteLength:            21,
					Digest:                testDigest([]byte("agent-request-payload")),
					Classification:        "synthetic",
					RequiredGrantAudience: "elitea.runtime.input.read.v1",
				},
			},
		},
	}
}

func claimRequestForAgentManifest(t *testing.T, manifest *runtimev1.ExecutionInputBundleV1) (*runtimev1.ClaimCommandRequestV1, *runtimev1.WorkerCommandV1) {
	t.Helper()
	manifestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cmd := validVerifierAgentCommand(
		executiondomain.AgentApplicationCapability,
		runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION,
	)
	cmd.CommandId = "command-1"
	cmd.IdempotencyKey = "outbox-1"
	cmd.ExecutionId = "execution-1"
	cmd.Generation = 1
	cmd.InputBundleRef = &runtimev1.ExecutionInputBundleReferenceV1{
		InputBundleId:    manifest.GetInputBundleId(),
		ImmutableVersion: manifest.GetImmutableVersion(),
		ByteLength:       uint64(len(manifestBytes)),
		Digest:           testDigest(manifestBytes),
		MediaType:        executiondomain.InputBundleManifestMediaType,
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.ClaimCommandRequestV1{
		WorkloadSessionId: "workload-1",
		ProducerId:        "worker-1",
		SignedCommand:     signedEnvelope(raw),
	}, cmd
}

func validToolRunManifest() *runtimev1.ExecutionInputBundleV1 {
	return &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    "bundle-tool-1",
		ImmutableVersion: "v1",
		Entries: []*runtimev1.ExecutionInputEntryV1{
			{
				EntryId:          "toolkit-settings",
				ImmutableVersion: "revision-1",
				SemanticRole:     executiondomain.ToolkitCallToolSettingsRole,
				Content: &runtimev1.ScopedContentReferenceV1{
					ContentId:             "content-settings",
					ImmutableVersion:      "revision-1",
					MediaType:             "application/json",
					ByteLength:            8,
					Digest:                testDigest([]byte("settings")),
					Classification:        "synthetic",
					RequiredGrantAudience: "elitea.runtime.input.read.v1",
				},
			},
			{
				EntryId:          "tool-arguments",
				ImmutableVersion: "revision-1",
				SemanticRole:     executiondomain.ToolkitCallToolArgumentsRole,
				Content: &runtimev1.ScopedContentReferenceV1{
					ContentId:             "content-arguments",
					ImmutableVersion:      "revision-1",
					MediaType:             executiondomain.SettingsJSONMediaType,
					ByteLength:            9,
					Digest:                testDigest([]byte("arguments")),
					Classification:        "synthetic",
					RequiredGrantAudience: "elitea.runtime.input.read.v1",
				},
			},
			{
				EntryId:          "toolkit-runtime-context",
				ImmutableVersion: "revision-1",
				SemanticRole:     executiondomain.ToolkitCallToolRuntimeContextRole,
				Content: &runtimev1.ScopedContentReferenceV1{
					ContentId:             "content-runtime-context",
					ImmutableVersion:      "revision-1",
					MediaType:             executiondomain.SettingsJSONMediaType,
					ByteLength:            2,
					Digest:                testDigest([]byte(`{}`)),
					Classification:        "synthetic",
					RequiredGrantAudience: "elitea.runtime.input.read.v1",
				},
			},
		},
	}
}

func claimRequestForToolRunManifest(t *testing.T, manifest *runtimev1.ExecutionInputBundleV1) (*runtimev1.ClaimCommandRequestV1, *runtimev1.WorkerCommandV1) {
	t.Helper()
	manifestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawWorkerCommand(t), cmd); err != nil {
		t.Fatal(err)
	}
	cmd.CapabilityId = executiondomain.ToolkitCallToolCapability
	cmd.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL
	cmd.CapabilityCommand = &runtimev1.WorkerCommandV1_ToolkitCallTool{
		ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{
			SettingsEntryId:  "toolkit-settings",
			ArgumentsEntryId: "tool-arguments",
		},
	}
	cmd.InputBundleRef = &runtimev1.ExecutionInputBundleReferenceV1{
		InputBundleId:    manifest.GetInputBundleId(),
		ImmutableVersion: manifest.GetImmutableVersion(),
		ByteLength:       uint64(len(manifestBytes)),
		Digest:           testDigest(manifestBytes),
		MediaType:        executiondomain.InputBundleManifestMediaType,
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.ClaimCommandRequestV1{
		WorkloadSessionId: "workload-1",
		ProducerId:        "worker-1",
		SignedCommand:     signedEnvelope(raw),
	}, cmd
}

type claimRepoFunc func(context.Context, executionapp.ClaimRequest, executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error)

func (f claimRepoFunc) ClaimValidation(ctx context.Context, req executionapp.ClaimRequest, ttl executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error) {
	return f(ctx, req, ttl)
}

func (f claimRepoFunc) CurrentLease(context.Context, string, uint64) (runtimedomain.ActiveLease, time.Time, error) {
	return runtimedomain.ActiveLease{}, time.Time{}, nil
}

func (f claimRepoFunc) RenewLease(context.Context, runtimedomain.Fence, executionapp.ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error) {
	return runtimedomain.ActiveLease{}, time.Time{}, nil
}

func (f claimRepoFunc) ReleaseClaim(context.Context, runtimedomain.Fence) error { return nil }
func (f claimRepoFunc) AbortClaim(context.Context, runtimedomain.Fence, executionapp.ClaimAbortDisposition) error {
	return nil
}
func (f claimRepoFunc) DesiredState(context.Context, string, uint64) (runtimedomain.DesiredState, error) {
	return "", nil
}
