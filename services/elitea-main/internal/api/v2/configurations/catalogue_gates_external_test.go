package configurations_test

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

// openCatalogueGatesExternal admit every caller: the integration tests that
// use them prove the stores, and the gates are proven through the production
// router (internal/api/admin_gateway_gates_test.go).
var openCatalogueGatesExternal = configurations.CatalogueGates{
	View:   func(next http.Handler) http.Handler { return next },
	Manage: func(next http.Handler) http.Handler { return next },
}
