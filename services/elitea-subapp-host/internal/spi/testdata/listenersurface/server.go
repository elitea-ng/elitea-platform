// Fixture for the source check (ADR-0059 R1.8): a new listener with a new
// service and an internal HTTP route, none of them on the allowlist.
package registration

import (
	"net/http"

	"google.golang.org/grpc"

	adminv1 "example.invalid/fixture/admin/v1"
)

func serveAdmin(implementation adminv1.FixtureAdminServiceServer) (*grpc.Server, *http.Server) {
	server := grpc.NewServer()
	adminv1.RegisterFixtureAdminServiceServer(server, implementation)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/roles", func(http.ResponseWriter, *http.Request) {})
	return server, &http.Server{Handler: mux}
}
