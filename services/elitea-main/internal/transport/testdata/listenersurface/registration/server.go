// Fixture for the source check (ADR-0059 R1.8): new listeners, a new
// service and internal HTTP routes, none of them reviewed.
package registration

import (
	"net"
	"net/http"

	"google.golang.org/grpc"

	adminv1 "example.invalid/fixture/admin/v1"
)

func serveAdmin(implementation adminv1.FixtureAdminServiceServer) (*grpc.Server, *grpc.Server, *http.Server) {
	server := grpc.NewServer()
	adminv1.RegisterFixtureAdminServiceServer(server, implementation)
	second := grpc.NewServer()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/roles", func(http.ResponseWriter, *http.Request) {})
	registerMore(mux)
	return server, second, &http.Server{Handler: mux}
}

func registerMore(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /admin/users", func(http.ResponseWriter, *http.Request) {})
}

func registerDefault() {
	http.HandleFunc("PUT /admin/policy", func(http.ResponseWriter, *http.Request) {})
}

func listenRaw() (net.Listener, error) { return net.Listen("tcp", ":0") }
