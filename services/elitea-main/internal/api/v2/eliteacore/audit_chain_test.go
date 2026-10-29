package eliteacore_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

const auditChainPath = "/elitea_core/audit_chain_verify/administration"

func auditChainRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get(auditChainPath, handler.AuditChainVerify)
	return router
}

// Parameters are refused before the store is consulted, and no store is a
// 503, never an empty "ok".
func TestAuditChainVerifyRefusesBadParametersAndNoStore(t *testing.T) {
	t.Parallel()

	router := auditChainRouter(eliteacore.NewHandler(nil))
	for query, want := range map[string]int{
		"?from=0":        http.StatusBadRequest,
		"?from=x":        http.StatusBadRequest,
		"?limit=0":       http.StatusBadRequest,
		"?limit=1000001": http.StatusBadRequest,
		"":               http.StatusServiceUnavailable,
		"?from=3&limit=": http.StatusServiceUnavailable,
	} {
		if got := auditGet(t, router, auditChainPath+query).Code; got != want {
			t.Errorf("GET %s%s = %d, want %d", auditChainPath, query, got, want)
		}
	}
}
