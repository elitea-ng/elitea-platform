package spi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// The platform service: operations that are not toolkit tools.
//
// A toolkit tool is reachable by any user session the facade vouches for.
// Some operations are not a user's to run: project deprovisioning deletes
// everything a project owns in the application. Those are exposed as the
// gRPC service elitea.subapp.v1.PlatformOperations (libs/proto), which is the
// transport AGENTS.md prescribes for internal service-to-service calls (gRPC
// over mTLS).
//
// It listens on a port of its own, behind the same TLS configuration as the
// SPI's listener (the server certificate, and the client CA with
// RequireAndVerifyClientCert), and every call is authorised by ONE thing: the
// verified mutual-TLS client certificate of the platform's own service
// (elitea-main), matched against the allowlist <PREFIX>PLATFORM_CLIENTS. With
// an empty allowlist the service is not served at all.
//
// What does NOT authorise it: an identity signature (the facade signs one for
// every user call, and "a signed hop with no user id" is not proof of
// anything), request metadata, or a request field. The SPI's HTTP listener
// carries user traffic only and has no route for these operations.

// DefaultPlatformGRPCAddr is where the service listens when
// <PREFIX>PLATFORM_CLIENTS is set and <PREFIX>PLATFORM_GRPC_ADDR is not.
const DefaultPlatformGRPCAddr = ":9443"

// Failures a platform operation reports as such, so the transport can give
// the caller a code it can act on.
var (
	// ErrGenerationRunning: a generation of the project (or wiki) is still
	// running after the bounded wait for it to stop. Nothing was deleted;
	// retry.
	ErrGenerationRunning = errors.New("a generation is still running; retry")
	// ErrBusy: a publish holds the wiki past the bounded wait. Nothing was
	// deleted; retry.
	ErrBusy = errors.New("the wiki is being published; retry")
)

// WikiDeletion is what one wiki's deletion removed.
type WikiDeletion struct {
	WikiID                               string
	Nodes, Edges, Embeddings, Statistics int64
}

// ProjectDeletion is what DeleteProject removed.
type ProjectDeletion struct {
	ProjectID          int32
	Wikis              []WikiDeletion
	LiveBuilds         int64
	StaleBuildsRemoved int64
	// Errors are problems that did not stop the deletion.
	Errors []string
}

// PlatformOps is what a runner offers the platform service.
type PlatformOps interface {
	// DeleteProject removes everything the application holds for the
	// project, and answers what was removed. It must be idempotent.
	DeleteProject(ctx context.Context, projectID int32) (*ProjectDeletion, error)
}

// PlatformClients parses <PREFIX>PLATFORM_CLIENTS: a comma-separated list of
// certificate identities (a subject common name or a DNS SAN).
func PlatformClients(raw string) []string {
	var clients []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			clients = append(clients, part)
		}
	}
	return clients
}

// errNoVerifiedPeer: the connection carries no verified client certificate.
var errNoVerifiedPeer = errors.New("no verified client certificate")

// verifiedPeerCertificate is the leaf of the VERIFIED chain of the gRPC
// connection ctx belongs to. A cleartext connection, a TLS connection with no
// verified chain, or a peer certificate that is not the chain's leaf: all
// refused. The rules are those of elitea-main's transport/workloadauth
// verifiedPeerIdentity (that package is internal to elitea-main and cannot be
// imported here, so they are mirrored): trust the chain the TLS stack
// verified, never a certificate the peer merely presented.
func verifiedPeerCertificate(ctx context.Context) (*x509.Certificate, error) {
	info, ok := peer.FromContext(ctx)
	if !ok || info == nil || info.AuthInfo == nil {
		return nil, errNoVerifiedPeer
	}
	var state tls.ConnectionState
	switch auth := info.AuthInfo.(type) {
	case credentials.TLSInfo:
		state = auth.State
	case *credentials.TLSInfo:
		if auth == nil {
			return nil, errNoVerifiedPeer
		}
		state = auth.State
	default:
		return nil, errNoVerifiedPeer
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 || state.VerifiedChains[0][0] == nil {
		return nil, errNoVerifiedPeer
	}
	leaf := state.VerifiedChains[0][0]
	if len(state.PeerCertificates) == 0 || state.PeerCertificates[0] == nil ||
		len(leaf.Raw) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, leaf.Raw) {
		return nil, errNoVerifiedPeer
	}
	return leaf, nil
}

// VerifiedPeerIdentity is the allowed identity (a subject common name or DNS
// SAN) of the verified client certificate behind ctx, or an error: Unauthenticated
// when the connection carries no verified certificate, PermissionDenied when
// the certificate is verified but names no allowed client (an empty allowlist
// allows nobody).
func VerifiedPeerIdentity(ctx context.Context, allowed []string) (string, error) {
	leaf, err := verifiedPeerCertificate(ctx)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "a verified client certificate is required")
	}
	identity, ok := matchClient(leaf, allowed)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "this client certificate is not allowed to call platform operations")
	}
	return identity, nil
}

func matchClient(leaf *x509.Certificate, allowed []string) (string, bool) {
	identities := append([]string{leaf.Subject.CommonName}, leaf.DNSNames...)
	for _, identity := range identities {
		if identity == "" {
			continue
		}
		for _, want := range allowed {
			if identity == want {
				return identity, true
			}
		}
	}
	return "", false
}

type platformCaller struct{}

type platformService struct {
	subappv1.UnimplementedPlatformOperationsServer
	ops    PlatformOps
	logger *slog.Logger
}

// NewPlatformGRPCServer is the platform service over tlsConfig, which must
// require and verify client certificates (the SPI listener's own
// configuration), authorising every call by the verified client certificate
// against clients. It refuses to build with no clients or a configuration
// that would accept a call without a certificate.
func NewPlatformGRPCServer(ops PlatformOps, clients []string, tlsConfig *tls.Config, logger *slog.Logger) (*grpc.Server, error) {
	switch {
	case ops == nil:
		return nil, fmt.Errorf("%w: the runner has no platform operations to serve", ErrConfig)
	case len(clients) == 0:
		return nil, fmt.Errorf("%w: the platform service needs an allowlist of client certificate identities", ErrConfig)
	case tlsConfig == nil || tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert || tlsConfig.ClientCAs == nil:
		return nil, fmt.Errorf("%w: the platform service needs a TLS configuration that requires and verifies client certificates (set the TLS certificate, key and client CA files)", ErrConfig)
	}
	if logger == nil {
		logger = slog.Default()
	}
	allowed := append([]string(nil), clients...)
	authorize := func(ctx context.Context, method string) (context.Context, error) {
		caller, err := VerifiedPeerIdentity(ctx, allowed)
		if err != nil {
			logger.Warn("refusing a platform operation", "method", method, "code", status.Code(err).String())
			return nil, err
		}
		return context.WithValue(ctx, platformCaller{}, caller), nil
	}
	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		// A request is one project id; nothing legitimate is larger.
		grpc.MaxRecvMsgSize(4096),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			ctx, err := authorize(ctx, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return handler(ctx, request)
		}),
		grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			// The service has no streaming method; an authorised caller
			// gets the same Unimplemented the framework would answer.
			if _, err := authorize(stream.Context(), info.FullMethod); err != nil {
				return err
			}
			return handler(srv, stream)
		}),
	)
	subappv1.RegisterPlatformOperationsServer(server, &platformService{ops: ops, logger: logger})
	return server, nil
}

func (p *platformService) DeleteProject(ctx context.Context, request *subappv1.DeleteProjectRequest) (*subappv1.DeleteProjectResponse, error) {
	project := request.GetProjectId()
	if project <= 0 {
		return nil, status.Error(codes.InvalidArgument, "project_id must be a positive integer")
	}
	caller, _ := ctx.Value(platformCaller{}).(string)
	p.logger.Info("platform operation", "op", "delete_project", "project_id", project, "caller", caller)
	result, err := p.ops.DeleteProject(ctx, project)
	if err != nil {
		p.logger.Error("platform delete_project failed", "project_id", project, "error", err)
		return nil, platformStatus(err)
	}
	response := &subappv1.DeleteProjectResponse{
		ProjectId:          project,
		LiveBuilds:         result.LiveBuilds,
		StaleBuildsRemoved: result.StaleBuildsRemoved,
		Errors:             result.Errors,
	}
	for _, wiki := range result.Wikis {
		response.Wikis = append(response.Wikis, &subappv1.WikiDeletion{
			WikiId: wiki.WikiID, Nodes: wiki.Nodes, Edges: wiki.Edges,
			Embeddings: wiki.Embeddings, Statistics: wiki.Statistics,
		})
	}
	return response, nil
}

// platformStatus gives a failure the gRPC code a caller can act on: retry
// for the two "not now" answers, a bad request for a bad value, a plain
// internal error otherwise. The message is the failure's text, which never
// carries a statement or a credential (the engine keeps those in its log).
func platformStatus(err error) error {
	switch {
	case errors.Is(err, ErrGenerationRunning):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrBusy):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "the call was cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "the call timed out")
	case KindOf(err) == KindValue:
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
