package output

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

func TestCodeNodeFailureHasActionablePublicPolicy(t *testing.T) {
	policy, ok := runtimeFailurePolicyFor(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED)
	if !ok || policy.Code != "PIPELINE_CODE_FAILED" || !strings.Contains(policy.SafeMessage, "Code node") || !strings.Contains(policy.SafeMessage, "Later nodes did not run") {
		t.Fatalf("Code failure policy missing: %+v", policy)
	}
}

func TestCodePreparationPoliciesCrossOutputBoundaryWithoutRawDetails(t *testing.T) {
	data, err := os.ReadFile("../../../../../../testdata/proto/runtime/v1/code_preparation_failure_policies.json")
	if err != nil {
		t.Fatal(err)
	}
	var policies []struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(data, &policies); err != nil {
		t.Fatal(err)
	}
	if len(policies) != 3 {
		t.Fatalf("unexpected preparation policy count: %d", len(policies))
	}
	for _, policy := range policies {
		for _, variant := range []string{"valid", "raw", "suffix", "retryable", "digest", "wrong-code"} {
			t.Run(policy.Message+"/"+variant, func(t *testing.T) {
				frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
				frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED
				frame.GetRuntimeError().SafeMessage = policy.Message
				frame.GetRuntimeError().Retryable = policy.Retryable
				switch variant {
				case "raw":
					frame.GetRuntimeError().SafeMessage = "SELECT private_schema; credential=secret"
				case "suffix":
					frame.GetRuntimeError().SafeMessage += " credential=secret"
				case "retryable":
					frame.GetRuntimeError().Retryable = true
				case "wrong-code":
					frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED
				}
				rebindFramePayload(t, frame, frame.GetRuntimeError())
				if variant == "digest" {
					frame.PayloadDigest.Value[0] ^= 1
				}
				failures := &failureIngestorStub{}
				server := newOutputTestServer(t, &validationIngestorStub{}, failures)
				stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
				if err := server.Publish(stream); err != nil {
					t.Fatal(err)
				}
				if variant != "valid" {
					if len(failures.frames) != 0 {
						t.Fatal("invalid failure persisted")
					}
					return
				}
				if len(failures.frames) != 1 {
					t.Fatalf("preparation failure rejected: %v", stream.acks)
				}
				got := failures.frames[0].Failure
				if got.Code != policy.Code || got.SafeMessage != policy.Message || got.Retryable {
					t.Fatalf("preparation category changed: %+v", got)
				}
			})
		}
	}
}

func TestCodeTerminalCategoriesCrossOutputBoundary(t *testing.T) {
	for _, policy := range []struct {
		code    runtimev1.RuntimeErrorCodeV1
		public  string
		message string
	}{
		{runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED, "PIPELINE_CODE_FAILED", "The pipeline stopped at a Code node. Check its source, selected input, and sandbox limits. Later nodes did not run. Share the support reference with your administrator before retrying."},
		{runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, "AUTHORIZATION_FAILED", "Execution authorization failed."},
		{runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED, "CANCELLED", "Execution was cancelled."},
	} {
		t.Run(policy.public, func(t *testing.T) {
			frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
			frame.GetRuntimeError().Code = policy.code
			frame.GetRuntimeError().SafeMessage = policy.message
			frame.GetRuntimeError().Retryable = false
			if policy.public == "CANCELLED" {
				frame.GetSettlementProposal().RequestedOutcome = runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED
			}
			rebindFramePayload(t, frame, frame.GetRuntimeError())
			failures := &failureIngestorStub{}
			server := newOutputTestServer(t, &validationIngestorStub{}, failures)
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(failures.frames) != 1 {
				t.Fatalf("terminal category rejected: %v", stream.acks)
			}
			got := failures.frames[0].Failure
			if got.Code != policy.public || got.SafeMessage != policy.message || got.Retryable {
				t.Fatalf("terminal category changed: %+v", got)
			}
		})
	}
}
