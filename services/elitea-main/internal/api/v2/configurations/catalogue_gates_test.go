package configurations

import "net/http"

// openCatalogueGates admit every caller. The tests that use them prove what a
// handler does once admitted; the gates are proven through the production
// router (internal/api/admin_gateway_gates_test.go) and gatedProviderRouter.
var openCatalogueGates = CatalogueGates{View: admitEveryone, Manage: admitEveryone}

func admitEveryone(next http.Handler) http.Handler { return next }
