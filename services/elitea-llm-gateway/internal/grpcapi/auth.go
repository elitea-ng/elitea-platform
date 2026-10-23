package grpcapi

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// errNoIdentity is a peer certificate that names no single workload identity.
var errNoIdentity = errors.New("the client certificate names no single workload identity")

// Allowlist is the set of workload identities admitted to GatewayService.
// Identities are spelled as elitea-main's internal/auth/workloadidentity
// spells them: "dns:<name>" for a certificate with exactly one DNS SAN, or the
// spiffe:// URI of a certificate with exactly one URI SAN.
type Allowlist struct {
	identities map[string]struct{}
}

// ParseAllowlist parses a comma-separated identity list (GATEWAY_GRPC_CLIENTS).
// A list that admits nobody is an error: a listener nobody may call is a
// configuration mistake, not a closed door.
func ParseAllowlist(raw string) (Allowlist, error) {
	identities := map[string]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !validIdentity(entry) {
			return Allowlist{}, fmt.Errorf("client %q is not a dns: or spiffe:// identity", entry)
		}
		identities[entry] = struct{}{}
	}
	if len(identities) == 0 {
		return Allowlist{}, errors.New("at least one client identity is required")
	}
	return Allowlist{identities: identities}, nil
}

func validIdentity(value string) bool {
	if len(value) > 512 || strings.ContainsAny(value, " \t\r\n\x00") {
		return false
	}
	if name, ok := strings.CutPrefix(value, "dns:"); ok {
		return name != ""
	}
	rest, ok := strings.CutPrefix(value, "spiffe://")
	return ok && strings.Contains(rest, "/") && !strings.HasPrefix(rest, "/")
}

// authorize admits the verified peer of ctx when its identity is listed.
func (a Allowlist) authorize(ctx context.Context) error {
	identity, err := verifiedPeerIdentity(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, "A verified client certificate is required.")
	}
	if _, ok := a.identities[identity]; !ok {
		return status.Error(codes.PermissionDenied, "This client may not call this service.")
	}
	return nil
}

// UnaryInterceptor and StreamInterceptor run authorize before any handler.
func (a Allowlist) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.authorize(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, request)
	}
}

func (a Allowlist) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := a.authorize(stream.Context()); err != nil {
			return err
		}
		return handler(server, stream)
	}
}

// verifiedPeerIdentity is the workload identity of the verified mTLS peer.
func verifiedPeerIdentity(ctx context.Context) (string, error) {
	info, ok := peer.FromContext(ctx)
	if !ok || info == nil || info.AuthInfo == nil {
		return "", errNoIdentity
	}
	var state credentials.TLSInfo
	switch tlsInfo := info.AuthInfo.(type) {
	case credentials.TLSInfo:
		state = tlsInfo
	case *credentials.TLSInfo:
		if tlsInfo == nil {
			return "", errNoIdentity
		}
		state = *tlsInfo
	default:
		return "", errNoIdentity
	}
	if len(state.State.VerifiedChains) == 0 || len(state.State.VerifiedChains[0]) == 0 {
		return "", errNoIdentity
	}
	return certificateIdentity(state.State.VerifiedChains[0][0])
}

// certificateIdentity is elitea-main's workloadidentity.Certificate rule:
// exactly one SPIFFE URI SAN or exactly one DNS SAN, never the common name.
func certificateIdentity(cert *x509.Certificate) (string, error) {
	if cert == nil || len(cert.EmailAddresses) != 0 || len(cert.IPAddresses) != 0 {
		return "", errNoIdentity
	}
	if len(cert.URIs) == 1 && len(cert.DNSNames) == 0 {
		u := cert.URIs[0]
		if u == nil || u.Scheme != "spiffe" || u.Host == "" || u.Path == "" || u.Path == "/" ||
			u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.RawPath != "" {
			return "", errNoIdentity
		}
		canonical := u.String()
		if len(canonical) > 512 || strings.ContainsAny(canonical, "\r\n\x00") {
			return "", errNoIdentity
		}
		return canonical, nil
	}
	if len(cert.URIs) == 0 && len(cert.DNSNames) == 1 {
		name := strings.ToLower(strings.TrimSuffix(cert.DNSNames[0], "."))
		if name == "" || strings.ContainsAny(name, "*\r\n\x00") || net.ParseIP(name) != nil || cert.VerifyHostname(name) != nil {
			return "", errNoIdentity
		}
		for _, c := range name {
			if c > 0x7f {
				return "", errNoIdentity
			}
		}
		return "dns:" + name, nil
	}
	return "", errNoIdentity
}
