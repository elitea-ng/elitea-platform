package control

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
)

var (
	sessionStoreDown   = fmt.Errorf("%w: %w", workloadauth.ErrWorkloadUnauthorized, workloadauth.ErrSessionStoreUnavailable)
	sessionNotAccepted = workloadauth.ErrWorkloadUnauthorized
)

type rejectionCarrier interface {
	GetRejection() *runtimev1.RuntimeErrorV1
}

// FU-275: the RPCs a Worker calls while a run is live. With the session
// registry unreadable (a database restart) each is still refused, with
// retryable DEPENDENCY_UNAVAILABLE; with the session refused for any other
// reason each stays AUTHENTICATION_FAILED, not retryable. Nothing is claimed,
// renewed or settled either way.
func TestControlRPCsAnswerAnUnreadableSessionStoreAsRetryable(t *testing.T) {
	fence := &runtimev1.ExecutionFenceV1{WorkloadSessionId: "session-1", ProducerId: "producer-1"}
	identity := &runtimev1.ExecutionIdentityV1{ExecutionId: "execution-1", Generation: 1}
	rpcs := map[string]func(*Server) (rejectionCarrier, error){
		"ClaimCommand": func(s *Server) (rejectionCarrier, error) {
			return s.ClaimCommand(context.Background(), &runtimev1.ClaimCommandRequestV1{WorkloadSessionId: "session-1", ProducerId: "producer-1"})
		},
		"BeginExecution": func(s *Server) (rejectionCarrier, error) {
			return s.BeginExecution(context.Background(), &runtimev1.BeginExecutionRequestV1{Identity: identity, Fence: fence})
		},
		"AuthorizeInvocation": func(s *Server) (rejectionCarrier, error) {
			return s.AuthorizeInvocation(context.Background(), &runtimev1.AuthorizeInvocationRequestV1{Identity: identity, Fence: fence})
		},
		"RenewLease": func(s *Server) (rejectionCarrier, error) {
			return s.RenewLease(context.Background(), &runtimev1.RenewLeaseRequestV1{IdempotencyKey: "renew-1", Fence: fence})
		},
		"ObserveDesiredState": func(s *Server) (rejectionCarrier, error) {
			return s.ObserveDesiredState(context.Background(), &runtimev1.ObserveDesiredStateRequestV1{Fence: fence})
		},
		"PrepareSettlement": func(s *Server) (rejectionCarrier, error) {
			return s.PrepareSettlement(context.Background(), &runtimev1.PrepareSettlementRequestV1{
				IdempotencyKey: "settle-1",
				Fence:          fence,
				Proposal:       &runtimev1.SettlementProposalV1{PrepareIdempotencyKey: "settle-1"},
			})
		},
	}
	for name, call := range rpcs {
		for _, tc := range []struct {
			refusal       error
			wantCode      runtimev1.RuntimeErrorCodeV1
			wantRetryable bool
		}{
			{sessionStoreDown, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, true},
			{sessionNotAccepted, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, false},
		} {
			t.Run(fmt.Sprintf("%s/%v", name, tc.wantCode), func(t *testing.T) {
				calls := []string{}
				server, err := NewServer(
					testControlServerConfig(),
					workloadAuthorizerStub{calls: &calls, err: tc.refusal},
					verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
					claimControllerStub{calls: &calls},
					inputResolverStub{calls: &calls},
					&settlementControllerStub{calls: &calls},
				)
				if err != nil {
					t.Fatal(err)
				}
				response, err := call(server)
				if err != nil {
					t.Fatalf("%s returned a transport error %v, want a rejection", name, err)
				}
				rejection := response.GetRejection()
				if rejection.GetCode() != tc.wantCode || rejection.GetRetryable() != tc.wantRetryable {
					t.Fatalf("%s rejection = %v retryable=%v, want %v retryable=%v", name, rejection.GetCode(), rejection.GetRetryable(), tc.wantCode, tc.wantRetryable)
				}
				if len(calls) != 1 || calls[0] != "authorize-peer" {
					t.Fatalf("%s did work after refusing the workload: %v", name, calls)
				}
				for _, leak := range []string{"session-1", "producer-1", "SQLSTATE"} {
					if strings.Contains(rejection.GetSafeMessage(), leak) {
						t.Fatalf("%s rejection message %q carries %q", name, rejection.GetSafeMessage(), leak)
					}
				}
			})
		}
	}
}

func TestStatusRPCsAnswerAnUnreadableSessionStoreAsUnavailable(t *testing.T) {
	if got := status.Code(workloadRefusalStatus(sessionStoreDown)); got != codes.Unavailable {
		t.Fatalf("store down = %v, want Unavailable", got)
	}
	for _, refusal := range []error{sessionNotAccepted, nil} {
		if got := status.Code(workloadRefusalStatus(refusal)); got != codes.Unauthenticated {
			t.Fatalf("refusal %v = %v, want Unauthenticated", refusal, got)
		}
	}
}

// Every function in the runtime gRPC transport that asks the workload
// authorizer must classify its refusal through the shared rule, so a new RPC
// cannot answer a database restart as "not authenticated" (FU-275).
func TestEveryWorkloadAuthorizationCallerClassifiesItsRefusal(t *testing.T) {
	root := ".."
	classifiers := []string{"workloadRefusal", "workloadRefusalStatus", "SessionStoreUnavailable"}
	callers := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			authorizes, classifies := false, false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name == "AuthorizeWorkload" || n.Sel.Name == "AuthorizeOutput" {
						authorizes = true
					}
					for _, name := range classifiers {
						if n.Sel.Name == name {
							classifies = true
						}
					}
				case *ast.Ident:
					for _, name := range classifiers {
						if n.Name == name {
							classifies = true
						}
					}
				}
				return true
			})
			if authorizes {
				callers++
				if !classifies {
					t.Errorf("%s: %s authorizes a workload but answers its refusal without workloadRefusal", path, fn.Name.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if callers < 18 {
		t.Fatalf("found %d authorizing functions, fewer than the 18 this gate was written for: it read an incomplete tree", callers)
	}
}
