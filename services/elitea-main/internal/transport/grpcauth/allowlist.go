// Package grpcauth authorizes the callers of elitea-main's internal gRPC
// services (#1359 decision D2).
//
// Every internal gRPC listener already requires a client certificate that
// chains to the runtime CA at the handshake. That admits the CONNECTION. This
// package decides which workload may call which SERVICE:
//
//	verified peer certificate
//	  -> workload identity (internal/auth/workloadidentity: exactly one
//	     spiffe:// URI SAN or one DNS SAN, spelled "spiffe://…" / "dns:<name>")
//	  -> the allowlist configured for the called service
//
// The pieces:
//
//   - Allowlist: the identities admitted to one service, parsed from
//     configuration (an *_CLIENTS env list, a chart value). Handlers that sit on
//     a shared listener without an interceptor (egressaudit,
//     vectorintrospection on the control listener) call Allowlist.Authorize.
//   - Policy: one Allowlist per fully qualified service name, installed as a
//     unary + stream interceptor pair (Policy.ServerOptions). A method of a
//     service the policy does not name is refused: fail closed.
//   - metadata helpers (metadata.go): the bearer token, the execution claim and
//     fence, and the signed X-Elitea-* identity tuple, as they travel on a gRPC
//     hop.
//
// Certificate identity alone never authorizes an EXECUTION: execution-bound
// RPCs still bind the claim and fence to the peer through
// transport/workloadauth.
package grpcauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
)

// ErrNoClients is returned by NewAllowlist for an empty client list. A
// service with nobody allowed is a configuration error, not a closed door.
var ErrNoClients = errors.New("at least one client identity is required")

// Allowlist is the set of workload identities admitted to one service. The
// zero value admits nobody.
type Allowlist struct {
	anyVerifiedPeer bool
	identities      map[string]struct{}
}

// NewAllowlist parses client identities as workloadidentity spells them:
// "dns:<name>" or a spiffe:// URI. Blank entries are skipped; anything else is
// refused, and so is a list that leaves no identity.
func NewAllowlist(clients []string) (Allowlist, error) {
	identities := make(map[string]struct{}, len(clients))
	for _, client := range clients {
		client = strings.TrimSpace(client)
		if client == "" {
			continue
		}
		if !ValidIdentity(client) {
			return Allowlist{}, fmt.Errorf("client %q is not a dns: or spiffe:// identity", client)
		}
		identities[client] = struct{}{}
	}
	if len(identities) == 0 {
		return Allowlist{}, ErrNoClients
	}
	return Allowlist{identities: identities}, nil
}

// AnyVerifiedPeer admits every caller whose certificate the listener
// verified and that carries a valid workload identity. It is for services
// that disclose nothing a runtime-CA workload may not know, such as
// grpc.health.v1.
func AnyVerifiedPeer() Allowlist { return Allowlist{anyVerifiedPeer: true} }

// ValidIdentity reports whether value is spelled as a configured client
// identity may be: "dns:<name>" or "spiffe://<trust domain>/<path>".
func ValidIdentity(value string) bool {
	if len(value) > 512 || strings.ContainsAny(value, " \t\r\n\x00") {
		return false
	}
	if name, ok := strings.CutPrefix(value, "dns:"); ok {
		return name != ""
	}
	rest, ok := strings.CutPrefix(value, "spiffe://")
	return ok && strings.Contains(rest, "/") && !strings.HasPrefix(rest, "/")
}

// Allows reports whether identity is admitted.
func (a Allowlist) Allows(identity string) bool {
	if identity == "" {
		return false
	}
	if a.anyVerifiedPeer {
		return true
	}
	_, ok := a.identities[identity]
	return ok
}

// Configured reports whether the allowlist admits anyone at all.
func (a Allowlist) Configured() bool { return a.anyVerifiedPeer || len(a.identities) > 0 }

// Authorize resolves the verified peer of ctx and checks it. It returns the
// identity, or a gRPC status error: Unauthenticated without a verified
// certificate identity, PermissionDenied for an identity not on the list.
func (a Allowlist) Authorize(ctx context.Context) (string, error) {
	identity, err := workloadauth.VerifiedPeerIdentity(ctx)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "A verified client certificate is required.")
	}
	if !a.Allows(identity) {
		return "", status.Error(codes.PermissionDenied, "This client may not call this service.")
	}
	return identity, nil
}
