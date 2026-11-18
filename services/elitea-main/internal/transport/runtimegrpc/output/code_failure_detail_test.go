package output

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"google.golang.org/protobuf/proto"
)

// #2227: a Code node whose program failed ends the run with its registered
// sentence and the tail of the program's stderr. Nothing else may follow the
// sentence: only the exact separator and a clean, bounded detail are admitted.
func TestCodeNodeFailureAdmitsOnlyABoundedProgramOutputTail(t *testing.T) {
	long := strings.Repeat("x", outputapp.MaxCodeFailureDetailRunes+1)
	exact := strings.Repeat("é", outputapp.MaxCodeFailureDetailRunes)
	for _, tc := range []struct {
		name    string
		message string
		admit   bool
	}{
		{"detail", codeFailedSafeMessage + outputapp.CodeFailureDetailSeparator + "Error: boom\n    at main.js:1", true},
		{"the bound is in characters", codeFailedSafeMessage + outputapp.CodeFailureDetailSeparator + exact, true},
		{"over the bound", codeFailedSafeMessage + outputapp.CodeFailureDetailSeparator + long, false},
		{"empty detail", codeFailedSafeMessage + outputapp.CodeFailureDetailSeparator, false},
		{"blank detail", codeFailedSafeMessage + outputapp.CodeFailureDetailSeparator + " \n", false},
		{"control character", codeFailedSafeMessage + outputapp.CodeFailureDetailSeparator + "bad\x1b[31m", false},
		{"no separator", codeFailedSafeMessage + " Error: boom", false},
		{"another sentence", "A different sentence." + outputapp.CodeFailureDetailSeparator + "Error: boom", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
			frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED
			frame.GetRuntimeError().SafeMessage = tc.message
			frame.GetRuntimeError().Retryable = false
			rebindFramePayload(t, frame, frame.GetRuntimeError())
			failures := &failureIngestorStub{}
			server := newOutputTestServer(t, &validationIngestorStub{}, failures)
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if !tc.admit {
				if len(failures.frames) != 0 {
					t.Fatalf("a refused Code failure was persisted: %q", failures.frames[0].Failure.SafeMessage)
				}
				return
			}
			if len(failures.frames) != 1 {
				t.Fatalf("the Code failure with its program output was refused: %v", stream.acks)
			}
			got := failures.frames[0].Failure
			if got.Code != "PIPELINE_CODE_FAILED" || got.SafeMessage != tc.message || got.Retryable {
				t.Fatalf("the Code failure changed in transit: %+v", got)
			}
		})
	}
}
