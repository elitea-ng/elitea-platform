package workspace

import (
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
)

// workloadRefusal: see the control package's. Only an unreadable session
// registry is retryable; every other refusal is AUTHENTICATION_FAILED.
func workloadRefusal(err error) (runtimev1.RuntimeErrorCodeV1, string, bool) {
	if workloadauth.SessionStoreUnavailable(err) {
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The workload session store is temporarily unavailable.", true
	}
	return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false
}
