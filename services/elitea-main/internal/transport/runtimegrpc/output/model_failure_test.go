package output

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

func TestModelFailurePoliciesCrossOutputBoundary(t *testing.T) {
	data, err := os.ReadFile("../../../../../../testdata/proto/runtime/v1/model_failure_policies.json")
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
	for _, policy := range policies {
		t.Run(policy.Code, func(t *testing.T) {
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
				if got.Code != policy.Code || got.SafeMessage != policy.Message || got.Retryable != policy.Retryable {
					t.Fatalf("reason lost: %+v", got)
				}
			}
		})
	}
}
