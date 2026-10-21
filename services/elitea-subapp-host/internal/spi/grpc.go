package spi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The provider service: the SPI over gRPC (elitea.subapp.v1.ProviderService,
// libs/proto). It replaced the HTTP surface in issue #1359 — internal
// service-to-service traffic is gRPC over mutual TLS — and serves the same
// documents: every body is the JSON the HTTP surface wrote, so the facade can
// hand it on to the browser unchanged.
//
// Who may call it is decided twice, as on the HTTP surface. The listener's
// interceptor admits only a verified client certificate the allowlist names
// (platform.go: the same <PREFIX>PLATFORM_CLIENTS as the platform
// operations, because the caller is the same elitea-main). Then the caller's
// identity — whose project, whose user — is read from the signed metadata
// and verified against the shared secret, exactly as the identity gate
// verified the headers: a call with a missing or forged signature is
// refused, and the runner sees only an identity the signature vouches for.

// The metadata keys the identity travels in: the header names, lower-cased,
// which is how gRPC spells metadata keys.
var identityMetadataKeys = func() []string {
	keys := make([]string, 0, len(IdentityHeaders))
	for _, header := range IdentityHeaders {
		keys = append(keys, strings.ToLower(header))
	}
	return keys
}()

// IdentityMetadata is the signed identity as gRPC metadata: what a caller
// attaches to a ProviderService call. The signature is the one SignHeaders
// computes; the keys are the header names, lower-cased.
func IdentityMetadata(identity Identity, secret []byte) metadata.MD {
	headers := http.Header{}
	SignHeaders(headers, identity, secret)
	return MetadataFromHeaders(headers)
}

// MetadataFromHeaders copies the identity headers present in h into metadata,
// unverified — for a caller that already holds signed headers.
func MetadataFromHeaders(h http.Header) metadata.MD {
	md := metadata.MD{}
	for _, header := range IdentityHeaders {
		if value := h.Get(header); value != "" {
			md.Set(strings.ToLower(header), value)
		}
	}
	return md
}

// headersFromMetadata is the inverse: the identity metadata of an incoming
// call as headers, so VerifySignature and IdentityFromHeaders read it the way
// they read a request. A key sent more than once is refused rather than
// guessed at: the signature covers one value.
func headersFromMetadata(md metadata.MD) (http.Header, bool) {
	headers := http.Header{}
	for i, key := range identityMetadataKeys {
		values := md.Get(key)
		switch len(values) {
		case 0:
		case 1:
			headers.Set(IdentityHeaders[i], values[0])
		default:
			return nil, false
		}
	}
	return headers, true
}

// verifiedIdentity reads and verifies the caller's identity. With no shared
// secret configured nothing can be verified and the identity is EMPTY — never
// the metadata's claim (a runner whose tools address tenant data refuses a
// call with no verified project). With a secret, a missing or invalid
// signature is refused unless optional is set (Health).
func (s *Server) verifiedIdentity(ctx context.Context, optional bool) (Identity, error) {
	secret := []byte(s.settings.IdentitySecret)
	md, _ := metadata.FromIncomingContext(ctx)
	headers, ok := headersFromMetadata(md)
	if len(secret) == 0 {
		return Identity{}, nil
	}
	if ok && headers.Get(HeaderSignature) != "" && VerifySignature(headers, secret) {
		return IdentityFromHeaders(headers), nil
	}
	if optional {
		return Identity{}, nil
	}
	return Identity{}, status.Error(codes.Unauthenticated, "a valid identity signature is required")
}

type providerService struct {
	subappv1.UnimplementedProviderServiceServer
	server *Server
	// followEvery is how often a followed invocation is read.
	followEvery time.Duration
}

func (p *providerService) Describe(ctx context.Context, _ *subappv1.DescribeRequest) (*subappv1.DescribeResponse, error) {
	if _, err := p.server.verifiedIdentity(ctx, false); err != nil {
		return nil, err
	}
	descriptor, err := p.server.DescriptorJSON()
	if err != nil {
		p.server.logger.Error("the descriptor could not be encoded", "error", err)
		return nil, status.Error(codes.Internal, "the descriptor could not be encoded")
	}
	return &subappv1.DescribeResponse{DescriptorJson: descriptor}, nil
}

func (p *providerService) Health(ctx context.Context, _ *subappv1.HealthRequest) (*subappv1.HealthResponse, error) {
	if _, err := p.server.verifiedIdentity(ctx, true); err != nil {
		return nil, err
	}
	document, err := json.Marshal(p.server.HealthDocument())
	if err != nil {
		return nil, status.Error(codes.Internal, "the health document could not be encoded")
	}
	ready, checks := p.server.Readiness(ctx)
	return &subappv1.HealthResponse{Status: "UP", HealthJson: document, Ready: ready, Checks: checks}, nil
}

func (p *providerService) Slots(ctx context.Context, _ *subappv1.SlotsRequest) (*subappv1.SlotsResponse, error) {
	if _, err := p.server.verifiedIdentity(ctx, false); err != nil {
		return nil, err
	}
	body, err := json.Marshal(p.server.SlotsDocument())
	if err != nil {
		return nil, status.Error(codes.Internal, "the slots report could not be encoded")
	}
	return &subappv1.SlotsResponse{SlotsJson: body}, nil
}

func (p *providerService) Invoke(request *subappv1.InvokeRequest, stream grpc.ServerStreamingServer[subappv1.InvokeEvent]) error {
	ctx := stream.Context()
	identity, err := p.server.verifiedIdentity(ctx, false)
	if err != nil {
		return err
	}
	if request.GetToolkit() == "" || request.GetTool() == "" {
		return status.Error(codes.InvalidArgument, "a toolkit and a tool are required")
	}
	raw := request.GetRequestJson()
	if len(raw) > MaxInvokeBytes {
		return status.Error(codes.InvalidArgument, "the request body is too large")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return status.Error(codes.InvalidArgument, "the request body must be a JSON object")
	}
	invocation, err := p.server.Accept(ctx, identity, request.GetToolkit(), request.GetTool(), body)
	if err != nil {
		p.server.logger.Error("failed to accept an invocation", "toolkit", request.GetToolkit(), "tool", request.GetTool(), "error", err)
		return status.Error(codes.Internal, "the invocation could not be accepted")
	}
	accepted, err := json.Marshal(AcceptedBody(invocation))
	if err != nil {
		return status.Error(codes.Internal, "the acceptance could not be encoded")
	}
	if err := stream.Send(&subappv1.InvokeEvent{InvocationId: invocation.ID, Status: wireStarted, BodyJson: accepted}); err != nil {
		return err
	}
	if !request.GetFollow() {
		return nil
	}
	return p.follow(stream, request.GetToolkit(), request.GetTool(), invocation.ID)
}

// follow streams an invocation's poll bodies until it ends: one whenever it
// produced events or changed status, and the terminal one last. The caller
// going away ends the stream, not the invocation.
func (p *providerService) follow(stream grpc.ServerStreamingServer[subappv1.InvokeEvent], toolkit, tool, id string) error {
	ctx := stream.Context()
	every := p.followEvery
	if every <= 0 {
		every = 100 * time.Millisecond
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	last := wireStarted
	for {
		body, err := p.server.manager.Poll(ctx, toolkit, tool, id)
		if err != nil {
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			p.server.logger.Error("following an invocation failed", "invocation", id, "error", err)
			return status.Error(codes.Internal, "the invocation could not be read")
		}
		if body == nil {
			return status.Error(codes.NotFound, "the invocation is gone")
		}
		state, _ := body["status"].(string)
		terminal := TerminalBody(body)
		if _, events := body["custom_events"]; events || state != last || terminal {
			encoded, err := json.Marshal(body)
			if err != nil {
				return status.Error(codes.Internal, "the invocation could not be encoded")
			}
			if err := stream.Send(&subappv1.InvokeEvent{InvocationId: id, Status: state, BodyJson: encoded, Terminal: terminal}); err != nil {
				return err
			}
			last = state
		}
		if terminal {
			return nil
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
	}
}

func (p *providerService) GetInvocation(ctx context.Context, request *subappv1.GetInvocationRequest) (*subappv1.GetInvocationResponse, error) {
	if _, err := p.server.verifiedIdentity(ctx, false); err != nil {
		return nil, err
	}
	body, err := p.server.manager.Poll(ctx, request.GetToolkit(), request.GetTool(), request.GetInvocationId())
	if err != nil {
		p.server.logger.Error("poll failed", "error", err)
		return nil, status.Error(codes.Internal, "the invocation could not be read")
	}
	if body == nil {
		return nil, status.Error(codes.NotFound, "Resource Not Found")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, status.Error(codes.Internal, "the invocation could not be encoded")
	}
	return &subappv1.GetInvocationResponse{BodyJson: encoded, Terminal: TerminalBody(body)}, nil
}

func (p *providerService) Cancel(ctx context.Context, request *subappv1.CancelRequest) (*subappv1.CancelResponse, error) {
	if _, err := p.server.verifiedIdentity(ctx, false); err != nil {
		return nil, err
	}
	known, err := p.server.manager.Cancel(ctx, request.GetToolkit(), request.GetTool(), request.GetInvocationId())
	if err != nil {
		p.server.logger.Error("cancel failed", "error", err)
		return nil, status.Error(codes.Internal, "the invocation could not be cancelled")
	}
	if !known {
		return nil, status.Error(codes.NotFound, "Resource Not Found")
	}
	return &subappv1.CancelResponse{}, nil
}

// errNoServer: NewGRPCServer was handed no host.
var errNoServer = errors.New("the provider service needs a host")
