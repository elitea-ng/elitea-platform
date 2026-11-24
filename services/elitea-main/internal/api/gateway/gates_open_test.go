package gateway

import "net/http"

// The handler tests here exercise what a handler does once admitted, so they
// mount the routes behind gates that admit everyone. The gates themselves are
// proven through the production router (internal/api/admin_gateway_gates_test.go)
// and the kind check below the route gate in gates_test.go.

func passGate(next http.Handler) http.Handler { return next }

var (
	openGovernanceGates = GovernanceGates{
		View: passGate, Manage: passGate,
		Holds: func(*http.Request, ...string) bool { return true },
	}
	openLLMProxyGates = LLMProxyGates{Usage: passGate, CatalogueView: passGate, CatalogueManage: passGate}
)
