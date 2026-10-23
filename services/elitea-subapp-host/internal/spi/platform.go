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
	"time"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
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
// It is served on the host's one gRPC listener beside the provider service
// (grpc.go), behind the server certificate and the client CA with
// RequireAndVerifyClientCert, and every call is authorised by ONE thing: the
// verified mutual-TLS client certificate of the platform's own service
// (elitea-main), matched against the allowlist <PREFIX>PLATFORM_CLIENTS.
//
// What does NOT authorise it: an identity signature (the facade signs one for
// every user call, and "a signed hop with no user id" is not proof of
// anything), request metadata, or a request field. The provider service has
// no method for these operations.

// DefaultPlatformGRPCAddr is where the host's gRPC listener (the provider
// service and the platform operations) listens when
// <PREFIX>PLATFORM_GRPC_ADDR is not set.
const DefaultPlatformGRPCAddr = ":9443"

// MaxConnectionAge is how long a client connection to the gRPC listener
// lives before the server sends GOAWAY and the client opens a new one
// (newAuthorizedGRPCServer).
const MaxConnectionAge = time.Minute

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
	// ErrIngestRunning: an ingest of the project (or toolkit) is still
	// running after the bounded wait for it to stop (nothing was deleted),
	// or, for ONE toolkit, started meanwhile and holds its graph (nothing
	// was deleted either). A project deletion that met such a toolkit is not
	// a failure: it answers OK with Incomplete true and the toolkit in
	// BusyToolkits. Retry.
	ErrIngestRunning = errors.New("an ingest is still running; retry")
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
	// GraphToolkits are the toolkits whose Inventory graph was deleted, and
	// Graphs what that removed in all. Empty for an application with no
	// graphs. ToolkitGraphs is the same per toolkit, in deletion order.
	GraphToolkits []int64
	Graphs        GraphDeletion
	ToolkitGraphs []ToolkitGraphDeletion
	// Incomplete is true when something of the project was left alone: an
	// Inventory toolkit's graph an ingest holds (BusyToolkits) or a
	// DeepWiki build whose heartbeat is live (LiveBuilds). What was deleted
	// stays deleted; the caller calls again. False — the zero value, and
	// what an application with nothing to leave behind reports — means
	// nothing of the project is left.
	Incomplete bool
	// BusyToolkits are the Inventory toolkits whose graph was not deleted
	// because an ingest (or another write) holds it.
	BusyToolkits []int64
}

// ToolkitGraphDeletion is what one toolkit's graph held, within a project
// deletion.
type ToolkitGraphDeletion struct {
	ToolkitID int64
	Removed   GraphDeletion
}

// GraphDeletion is what the deletion of Inventory graphs removed.
type GraphDeletion struct {
	Entities, Relations, Sources, Documents int64
}

// ToolkitDeletion is what DeleteToolkit removed.
type ToolkitDeletion struct {
	ProjectID, ToolkitID int32
	// Deleted is false when the toolkit had no graph.
	Deleted bool
	Removed GraphDeletion
}

// PlatformOps is what a runner offers the platform service.
type PlatformOps interface {
	// DeleteProject removes everything the application holds for the
	// project, and answers what was removed. It must be idempotent.
	DeleteProject(ctx context.Context, projectID int32) (*ProjectDeletion, error)
}

// ToolkitOps is what a runner offers when the application keeps something per
// toolkit that the platform deletes with the toolkit (Inventory's graph).
// A runner that does not implement it leaves DeleteToolkit to the generated
// Unimplemented server, which answers UNIMPLEMENTED: DeepWiki's index is per
// wiki, and a wiki is deleted by its own tool.
type ToolkitOps interface {
	// DeleteToolkit removes what the application holds for one toolkit of
	// one project. It must be idempotent.
	DeleteToolkit(ctx context.Context, projectID, toolkitID int32) (*ToolkitDeletion, error)
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

// toolkitService adds DeleteToolkit to the platform service, for a runner
// that has per-toolkit data (ToolkitOps). Without it the service is the
// platformService alone and DeleteToolkit is the embedded generated
// Unimplemented one.
type toolkitService struct {
	*platformService
	toolkits ToolkitOps
}

// NewGRPCServer is the host's one gRPC listener: the provider service
// (grpc.go) and, when the runner has any, the platform operations — both
// over tlsConfig, which must require and verify client certificates, and
// both behind one authorisation: the verified client certificate, matched
// against clients. It refuses to build with no clients or a configuration
// that would accept a call without a certificate.
func NewGRPCServer(server *Server, clients []string, tlsConfig *tls.Config, logger *slog.Logger) (*grpc.Server, error) {
	if server == nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, errNoServer)
	}
	if logger == nil {
		logger = server.logger
	}
	grpcServer, err := newAuthorizedGRPCServer(clients, tlsConfig, logger,
		// An invoke body (MaxInvokeBytes) plus the message's own framing.
		MaxInvokeBytes+64<<10)
	if err != nil {
		return nil, err
	}
	subappv1.RegisterProviderServiceServer(grpcServer, &providerService{server: server})
	if ops, ok := server.PlatformOps(); ok {
		registerPlatform(grpcServer, ops, logger)
	}
	return grpcServer, nil
}

// NewPlatformGRPCServer is the platform operations alone over tlsConfig,
// authorised as NewGRPCServer authorises: for a runner tested without a host.
func NewPlatformGRPCServer(ops PlatformOps, clients []string, tlsConfig *tls.Config, logger *slog.Logger) (*grpc.Server, error) {
	if ops == nil {
		return nil, fmt.Errorf("%w: the runner has no platform operations to serve", ErrConfig)
	}
	if logger == nil {
		logger = slog.Default()
	}
	// A request is a project id and a toolkit id; nothing legitimate is larger.
	server, err := newAuthorizedGRPCServer(clients, tlsConfig, logger, 4096)
	if err != nil {
		return nil, err
	}
	registerPlatform(server, ops, logger)
	return server, nil
}

func registerPlatform(server *grpc.Server, ops PlatformOps, logger *slog.Logger) {
	base := &platformService{ops: ops, logger: logger}
	if toolkits, ok := ops.(ToolkitOps); ok {
		subappv1.RegisterPlatformOperationsServer(server, &toolkitService{platformService: base, toolkits: toolkits})
	} else {
		subappv1.RegisterPlatformOperationsServer(server, base)
	}
}

// newAuthorizedGRPCServer is a gRPC server over tlsConfig whose every call,
// unary or streaming, is authorised by the verified client certificate
// against clients before any handler runs.
func newAuthorizedGRPCServer(clients []string, tlsConfig *tls.Config, logger *slog.Logger, maxRecv int) (*grpc.Server, error) {
	switch {
	case len(clients) == 0:
		return nil, fmt.Errorf("%w: the gRPC listener needs an allowlist of client certificate identities (PLATFORM_CLIENTS)", ErrConfig)
	case tlsConfig == nil || tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert || tlsConfig.ClientCAs == nil:
		return nil, fmt.Errorf("%w: the gRPC listener needs a TLS configuration that requires and verifies client certificates (set the TLS certificate, key and client CA files)", ErrConfig)
	}
	allowed := append([]string(nil), clients...)
	authorize := func(ctx context.Context, method string) (context.Context, error) {
		caller, err := VerifiedPeerIdentity(ctx, allowed)
		if err != nil {
			logger.Warn("refusing a call", "method", method, "code", status.Code(err).String())
			return nil, err
		}
		return context.WithValue(ctx, platformCaller{}, caller), nil
	}
	return grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.MaxRecvMsgSize(maxRecv),
		// elitea-main reaches the host through its Service on one HTTP/2
		// connection, which stays on the replica it first landed on whatever
		// that replica's readiness says. The age makes the client reconnect,
		// and a reconnect goes to a ready replica, so one that lost its owner
		// claim (#1359) stops receiving work within the age. The grace is
		// unlimited (grpc-go's default): a call still running on the old
		// connection finishes.
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: MaxConnectionAge}),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			ctx, err := authorize(ctx, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return handler(ctx, request)
		}),
		grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if _, err := authorize(stream.Context(), info.FullMethod); err != nil {
				return err
			}
			return handler(srv, stream)
		}),
	), nil
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
		GraphToolkitIds:    result.GraphToolkits,
		Incomplete:         result.Incomplete,
		BusyToolkitIds:     result.BusyToolkits,
	}
	if len(result.GraphToolkits) > 0 {
		response.Graphs = graphDeletionOf(result.Graphs)
	}
	for _, toolkit := range result.ToolkitGraphs {
		response.ToolkitGraphs = append(response.ToolkitGraphs, &subappv1.ToolkitGraphDeletion{
			ToolkitId: toolkit.ToolkitID, Removed: graphDeletionOf(toolkit.Removed),
		})
	}
	if result.Incomplete {
		p.logger.Warn("platform delete_project is not complete; the caller retries",
			"project_id", project, "busy_toolkits", result.BusyToolkits, "live_builds", result.LiveBuilds)
	}
	for _, wiki := range result.Wikis {
		response.Wikis = append(response.Wikis, &subappv1.WikiDeletion{
			WikiId: wiki.WikiID, Nodes: wiki.Nodes, Edges: wiki.Edges,
			Embeddings: wiki.Embeddings, Statistics: wiki.Statistics,
		})
	}
	return response, nil
}

func graphDeletionOf(removed GraphDeletion) *subappv1.GraphDeletion {
	return &subappv1.GraphDeletion{
		Entities: removed.Entities, Relations: removed.Relations,
		Sources: removed.Sources, Documents: removed.Documents,
	}
}

func (t *toolkitService) DeleteToolkit(ctx context.Context, request *subappv1.DeleteToolkitRequest) (*subappv1.DeleteToolkitResponse, error) {
	project, toolkit := request.GetProjectId(), request.GetToolkitId()
	if project <= 0 {
		return nil, status.Error(codes.InvalidArgument, "project_id must be a positive integer")
	}
	if toolkit <= 0 {
		return nil, status.Error(codes.InvalidArgument, "toolkit_id must be a positive integer")
	}
	caller, _ := ctx.Value(platformCaller{}).(string)
	t.logger.Info("platform operation", "op", "delete_toolkit", "project_id", project, "toolkit_id", toolkit, "caller", caller)
	result, err := t.toolkits.DeleteToolkit(ctx, project, toolkit)
	if err != nil {
		t.logger.Error("platform delete_toolkit failed", "project_id", project, "toolkit_id", toolkit, "error", err)
		return nil, platformStatus(err)
	}
	return &subappv1.DeleteToolkitResponse{
		ProjectId: project, ToolkitId: toolkit,
		Deleted: result.Deleted, Removed: graphDeletionOf(result.Removed),
	}, nil
}

// platformStatus gives a failure the gRPC code a caller can act on: retry
// for the two "not now" answers, a bad request for a bad value, a plain
// internal error otherwise. The message is the failure's text, which never
// carries a statement or a credential (the engine keeps those in its log).
func platformStatus(err error) error {
	switch {
	case errors.Is(err, ErrGenerationRunning), errors.Is(err, ErrIngestRunning):
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
