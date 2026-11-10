package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/logx"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// B3: a registration still contended after its bounded retries is answered
// retryable with the registry code in its status, and writes one WARN fault
// line; a refusal stays a refusal, logged as code.registration_refused (B6).
func TestAContendedCodeRegistrationIsRetryableAndLoggedOnce(t *testing.T) {
	var out bytes.Buffer
	server := &ContentServer{logger: slog.New(logx.NewHandler(&out, logx.Options{}))}
	claim := ContentClaim{ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1}
	contended := fmt.Errorf("%w after 3 attempts: %w", code.ErrRegistrationContended, errors.New("SQLSTATE 40001"))
	status, _ := grpcstatus.FromError(server.originalCodeStatus(t.Context(), claim, "visit", contended))
	if status.Code() != codes.Unavailable || !strings.Contains(status.Message(), "("+faults.CodeRegistrationContended.String()+")") {
		t.Fatalf("status = %v", status)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one fault line, got %q", out.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatal(err)
	}
	if line["level"] != "WARN" || line["code"] != faults.CodeRegistrationContended.String() || line["msg"] != faults.CodeRegistrationContended.Sentence() || line["execution_id"] != claim.ExecutionID || line["registration"] != "visit" {
		t.Fatalf("fault line = %v", line)
	}
	out.Reset()
	refused, _ := grpcstatus.FromError(server.originalCodeStatus(t.Context(), claim, "intent", code.ErrRejected))
	if refused.Code() != codes.Aborted || !strings.Contains(out.String(), faults.CodeRegistrationRefused.String()) {
		t.Fatalf("a refusal changed or was not logged (B6): %v %q", refused, out.String())
	}
}

// stagedRefusal is a repository refusal that names its step, as
// repos.codeRegistrationStage does.
type stagedRefusal struct {
	stage string
	err   error
}

func (e stagedRefusal) Error() string { return e.stage + ": " + e.err.Error() }
func (e stagedRefusal) Unwrap() error { return e.err }
func (e stagedRefusal) Stage() string { return e.stage }

// refusingCodeIntents answers every registration with one error.
type refusingCodeIntents struct{ err error }

func (r refusingCodeIntents) RegisterOriginalCodeVisit(context.Context, ContentClaim, code.VisitRequest) (code.VisitResponse, error) {
	return code.VisitResponse{}, r.err
}
func (r refusingCodeIntents) RegisterOriginalCodeIntent(context.Context, ContentClaim, code.IntentRequest) (code.SignedGrant, error) {
	return code.SignedGrant{}, r.err
}

// B6 log capture: every refused or failed Code visit or intent registration,
// at the handler, writes exactly one WARN fault line naming its registry
// code, sentence, stage, safe cause, ids and support_ref; the Code source,
// input and configuration (carrying a marker here) never reach the log.
func TestEveryRefusedOrFailedCodeRegistrationWritesOneFaultLine(t *testing.T) {
	const marker = "secret_code_marker"
	execution := "0123456789abcdef0123456789abcdef"
	fence := bytes.Repeat([]byte{3}, 32)
	_, leaf := sandboxTestCertificate(t, "worker.test")
	configuration := base64.RawURLEncoding.EncodeToString([]byte(`{"id":"shout","type":"code","code":{"type":"fixed","value":"print('` + marker + `')"}}`))
	prepared := base64.RawURLEncoding.EncodeToString([]byte(`{"source":"print('` + marker + `')","input":{"entity":"` + marker + `"}}`))
	visit, _ := json.Marshal(code.VisitRequest{Schema: "elitea.sandbox.original-code-visit-request.v1", ActivationID: strings.Repeat("1", 64), NodeID: "shout", GraphThread: "m1:item-0", Step: 1, Attempt: 1,
		NodeDigest: strings.Repeat("2", 64), OwningYAMLSHA256: strings.Repeat("3", 64), ConfigurationBase64URL: configuration, PreWorkspacePreparedBase64URL: prepared, SavedChildScope: json.RawMessage("null")})
	ref := code.OriginalVisitRef{VisitID: strings.Repeat("4", 64), Revision: 1, DigestSHA256: strings.Repeat("5", 64)}
	intent, _ := json.Marshal(code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: ref, DispatchActivation: strings.Repeat("6", 64), RequestDigest: strings.Repeat("7", 64), SupervisorAudience: "spiffe://elitea/supervisor/one", PreparedBase64URL: prepared})
	cases := []struct {
		name   string
		err    error
		status codes.Code
		code   faults.Code
		stage  string
	}{
		{"refused claim", stagedRefusal{"claim", ErrContentUnauthorized}, codes.Aborted, faults.CodeRegistrationRefused, "claim"},
		{"refused input", stagedRefusal{"input", code.ErrRejected}, codes.Aborted, faults.CodeRegistrationRefused, "input"},
		{"failed dependency", stagedRefusal{"visit_insert", errors.New("connection reset")}, codes.Unavailable, faults.CodeRegistrationFailed, "visit_insert"},
		{"contended", fmt.Errorf("%w after 3 attempts", code.ErrRegistrationContended), codes.Unavailable, faults.CodeRegistrationContended, "unknown"},
	}
	for _, tc := range cases {
		for _, registration := range []string{"visit", "intent"} {
			var out bytes.Buffer
			server := &ContentServer{logger: slog.New(logx.NewHandler(&out, logx.Options{})), requests: make(chan struct{}, 1), originalCodeIntents: refusingCodeIntents{tc.err}}
			ctx := contentClaimContext(t.Context(), leaf, "23456789abcdef0123456789abcdef01", fence)
			var err error
			if registration == "visit" {
				_, err = server.RegisterCodeVisit(ctx, &runtimev1.RegisterCodeVisitRequestV1{Scope: contentScope(execution, 1), VisitJson: visit})
			} else {
				_, err = server.RegisterCodeIntent(ctx, &runtimev1.RegisterCodeIntentRequestV1{Scope: contentScope(execution, 1), IntentJson: intent})
			}
			name := tc.name + "/" + registration
			if grpcstatus.Code(err) != tc.status {
				t.Fatalf("%s: status %v", name, err)
			}
			if strings.Contains(out.String(), marker) {
				t.Fatalf("%s: Code content reached the log: %s", name, out.String())
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("%s: want one fault line, got %q", name, out.String())
			}
			var line map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]any{"level": "WARN", "code": tc.code.String(), "msg": tc.code.Sentence(), "registration": registration, "stage": tc.stage, "execution_id": execution, "support_ref": execution} {
				if line[field] != want {
					t.Fatalf("%s: %s = %v, want %v (%v)", name, field, line[field], want, line)
				}
			}
			if line["cause"] == nil {
				t.Fatalf("%s: no safe cause: %v", name, line)
			}
			if registration == "visit" && (line["node_id"] != "shout" || line["graph_thread"] != "m1:item-0" || line["activation_id"] != strings.Repeat("1", 64)) {
				t.Fatalf("%s: visit ids missing: %v", name, line)
			}
			if registration == "intent" && (line["visit_id"] != ref.VisitID || line["dispatch_activation"] != strings.Repeat("6", 64)) {
				t.Fatalf("%s: intent ids missing: %v", name, line)
			}
		}
	}
}
