package grpcauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Policy maps fully qualified service names ("elitea.llm.v1.ModelService") to
// the Allowlist of each. It is immutable once built.
type Policy struct {
	services map[string]Allowlist
}

// NewPolicy builds a policy. Every entry needs a service name and an
// allowlist that admits someone.
func NewPolicy(services map[string]Allowlist) (*Policy, error) {
	copied := make(map[string]Allowlist, len(services))
	for name, allowlist := range services {
		if name == "" || strings.ContainsAny(name, "/ \r\n\x00") {
			return nil, fmt.Errorf("service name %q is invalid", name)
		}
		if !allowlist.Configured() {
			return nil, fmt.Errorf("service %s: %w", name, ErrNoClients)
		}
		copied[name] = allowlist
	}
	if len(copied) == 0 {
		return nil, errors.New("a gRPC authorization policy needs at least one service")
	}
	return &Policy{services: copied}, nil
}

// ServerOptions installs the unary and stream interceptors. Pass them to
// grpc.NewServer together with the listener's TLS credentials.
func (p *Policy) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(p.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(p.StreamInterceptor()),
	}
}

// Authorize checks the peer of ctx against the allowlist of the service that
// fullMethod ("/pkg.Service/Method") names, and returns a context that
// carries the caller's identity (PeerIdentity).
func (p *Policy) Authorize(ctx context.Context, fullMethod string) (context.Context, error) {
	service, ok := serviceOf(fullMethod)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "This method is not served here.")
	}
	allowlist, ok := p.services[service]
	if !ok {
		// Fail closed: a service registered on the server without a policy
		// entry is unreachable rather than open.
		return nil, status.Error(codes.PermissionDenied, "This method is not served here.")
	}
	identity, err := allowlist.Authorize(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, peerIdentityKey{}, identity), nil
}

// UnaryInterceptor authorizes every unary call before the handler runs.
func (p *Policy) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		authorized, err := p.Authorize(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(authorized, request)
	}
}

// StreamInterceptor authorizes every stream before the handler reads a
// message.
func (p *Policy) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authorized, err := p.Authorize(stream.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(server, &authorizedStream{ServerStream: stream, ctx: authorized})
	}
}

type peerIdentityKey struct{}

// PeerIdentity is the workload identity the interceptor admitted for this
// call. It is absent on a context that did not pass through a Policy.
func PeerIdentity(ctx context.Context) (string, bool) {
	identity, ok := ctx.Value(peerIdentityKey{}).(string)
	return identity, ok && identity != ""
}

type authorizedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authorizedStream) Context() context.Context { return s.ctx }

func serviceOf(fullMethod string) (string, bool) {
	trimmed, ok := strings.CutPrefix(fullMethod, "/")
	if !ok {
		return "", false
	}
	service, method, ok := strings.Cut(trimmed, "/")
	if !ok || service == "" || method == "" || strings.Contains(method, "/") {
		return "", false
	}
	return service, true
}
