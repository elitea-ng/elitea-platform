package output

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

func TestModelFailurePoliciesCrossOutputBoundary(t *testing.T) {
	data, err := os.ReadFile("../../../../../../testdata/proto/runtime/v1/model_failure_policies.json")
	if err != nil {
		t.Fatal(err)
	}
	var policies []struct {
		Code       string `json:"code"`
		PublicCode string `json:"public_code"`
		Message    string `json:"message"`
		Retryable  bool   `json:"retryable"`
	}
	if err := json.Unmarshal(data, &policies); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies {
		// A scoped budget refusal (#6732) shares its wire code with the
		// unscoped one, and Main reports it under its own public code.
		publicCode := policy.Code
		if policy.PublicCode != "" {
			publicCode = policy.PublicCode
		}
		t.Run(publicCode, func(t *testing.T) {
			for _, injected := range []bool{false, true} {
				frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
				code, ok := runtimev1.RuntimeErrorCodeV1_value["RUNTIME_ERROR_CODE_V1_"+policy.Code]
				if !ok {
					t.Fatalf("missing enum %s", policy.Code)
				}
				frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1(code)
				frame.GetRuntimeError().SafeMessage = policy.Message
				frame.GetRuntimeError().Retryable = policy.Retryable
				if injected {
					frame.GetRuntimeError().SafeMessage = "raw provider secret"
				}
				rebindFramePayload(t, frame, frame.GetRuntimeError())
				failures := &failureIngestorStub{}
				server := newOutputTestServer(t, &validationIngestorStub{}, failures)
				stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
				if err := server.Publish(stream); err != nil {
					t.Fatal(err)
				}
				if injected {
					if len(failures.frames) != 0 {
						t.Fatal("untrusted message persisted")
					}
					return
				}
				if len(failures.frames) != 1 {
					t.Fatalf("failure rejected: %v", stream.acks)
				}
				got := failures.frames[0].Failure
				if got.Code != publicCode || got.SafeMessage != policy.Message || got.Retryable != policy.Retryable {
					t.Fatalf("reason lost: %+v", got)
				}
			}
		})
	}
}

// Every runtime error code is either a run terminal with a registered,
// readable text, or one of the pinned codes that never reaches a run's
// terminal (claim, authentication and protocol refusals). A new code must be
// placed on one side (PM-2: RECOVERY_FRONTIER_UNSUPPORTED is a terminal).
func TestEveryRuntimeErrorCodeHasRegisteredTextOrIsNotATerminal(t *testing.T) {
	notTerminal := map[string]bool{
		"RUNTIME_ERROR_CODE_V1_UNSPECIFIED":              true,
		"RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED":    true,
		"RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION":       true,
		"RUNTIME_ERROR_CODE_V1_STALE_FENCE":              true,
		"RUNTIME_ERROR_CODE_V1_GOVERNED_WORKER_REQUIRED": true,
		"RUNTIME_ERROR_CODE_V1_GOVERNED_CONTEXT_MISSING": true,
	}
	for name, value := range runtimev1.RuntimeErrorCodeV1_value {
		policy, ok := runtimeFailurePolicyFor(runtimev1.RuntimeErrorCodeV1(value))
		switch {
		case ok && notTerminal[name]:
			t.Errorf("%s is pinned as not a terminal but has a policy", name)
		case !ok && !notTerminal[name]:
			t.Errorf("%s has no registered run-terminal text", name)
		case ok && (policy.Code == "" || policy.SafeMessage == ""):
			t.Errorf("%s maps to an empty policy", name)
		}
	}
}
