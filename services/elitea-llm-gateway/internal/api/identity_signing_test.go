package api

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// testIdentitySecret is the identity secret these router tests give the
// Handler. The gateway refuses every unsigned identity, so each request a test
// serves goes through signedRequest (or signingHandler for a real listener),
// which plays the edge's part and signs whatever identity headers the test set.
var testIdentitySecret = []byte("router-tests-identity-secret-0123456789")

func signedRequest(r *http.Request) *http.Request {
	llmproxy.SignIdentityHeaders(r.Header, testIdentitySecret,
		r.Header.Get("X-Elitea-Project-Id"), r.Header.Get("X-Elitea-User-Id"),
		r.Header.Get("X-Elitea-Tenant-Id"), r.Header.Get("X-Elitea-Execution-Id"))
	return r
}

func signingHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, signedRequest(r))
	})
}
