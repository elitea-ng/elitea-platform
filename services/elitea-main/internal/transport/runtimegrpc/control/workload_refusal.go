package control

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
)

// workloadRefusal is how every control RPC answers a workload it did not
// authorize. The request is refused either way and has done nothing. Only a
// session registry that could not be read because the database is restarting
// or recovering is answered retryable DEPENDENCY_UNAVAILABLE, so the Worker
// retries within its lease; an unknown, expired, revoked or foreign session
// stays AUTHENTICATION_FAILED, not retryable (FU-275).
func workloadRefusal(err error) (runtimev1.RuntimeErrorCodeV1, string, bool) {
	if workloadauth.SessionStoreUnavailable(err) {
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The workload session store is temporarily unavailable.", true
	}
	return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false
}

// workloadRefusalStatus is workloadRefusal for the RPCs that answer with a
// gRPC status: Unavailable is gRPC's retryable code, Unauthenticated is not.
func workloadRefusalStatus(err error) error {
	if workloadauth.SessionStoreUnavailable(err) {
		return status.Error(codes.Unavailable, "The workload session store is temporarily unavailable.")
	}
	return status.Error(codes.Unauthenticated, "The workload session is not accepted.")
}
