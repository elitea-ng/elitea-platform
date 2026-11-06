package storage

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"math"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

// browserViewGrantDomain is the supervisor's job grant signature domain
// (ADR-0049 decision 4): a view grant is a revision 1 sandbox job grant.
const browserViewGrantDomain = "elitea.sandbox.job-grant.ed25519.v1\x00"

// browserViewGrantLifetime is the grant's life: it authorizes the open frame
// only, so it never needs to outlive the dial.
const browserViewGrantLifetime = 30 * time.Second

// browserViewMessageBytes caps one view message: a frame is a JPEG of a
// viewport (the supervisor bounds it below this).
const browserViewMessageBytes = 4 << 20

// ErrBrowserViewRejected is a configuration, session or grant the client
// refuses before it dials.
var ErrBrowserViewRejected = errors.New("browser view rejected")

// BrowserViewEndpoint is one browser profile's supervisor.
type BrowserViewEndpoint struct {
	Audience string
	// Origin is the supervisor's https://host:port gRPC listener.
	Origin string
	TLS    *tls.Config
}

// BrowserViewClient opens BrowserView streams for the cloud browser view
// relay (#1597). It holds one gRPC connection (mTLS, ADR-0036) per
// configured supervisor, and signs each stream's view grant with Main's
// sandbox key: the session's run, the session's name as the activation,
// the view digest (which binds `control`), Main's own workload identity as
// the submitter. It calls BrowserView only: it never starts, drives or
// destroys a session.
type BrowserViewClient struct {
	keyID     string
	key       ed25519.PrivateKey
	requester string
	maxFPS    uint32
	endpoints map[string]browserViewEndpoint
	now       func() time.Time
}

type browserViewEndpoint struct {
	conn   *grpc.ClientConn
	client runtimev1.BrowserSessionServiceClient
}

// BrowserViewClientConfig is the composed client's identity.
type BrowserViewClientConfig struct {
	KeyID string
	Key   ed25519.PrivateKey
	// Requester is Main's workload identity: its client certificate's, and
	// the grant's submitter.
	Requester string
	// MaxFPS is the stream's frame rate; 0 is the supervisor's 10.
	MaxFPS    uint32
	Endpoints []BrowserViewEndpoint
}

// NewBrowserViewClient dials no one yet: grpc.NewClient connects lazily.
func NewBrowserViewClient(cfg BrowserViewClientConfig) (*BrowserViewClient, error) {
	if !browserViewIdentity(cfg.KeyID) || len(cfg.Key) != ed25519.PrivateKeySize || !browserViewIdentity(cfg.Requester) ||
		cfg.MaxFPS > 10 || len(cfg.Endpoints) == 0 || len(cfg.Endpoints) > 16 {
		return nil, ErrBrowserViewRejected
	}
	c := &BrowserViewClient{
		keyID: cfg.KeyID, key: append(ed25519.PrivateKey(nil), cfg.Key...), requester: cfg.Requester,
		maxFPS: cfg.MaxFPS, endpoints: map[string]browserViewEndpoint{}, now: time.Now,
	}
	for _, endpoint := range cfg.Endpoints {
		conn, err := browserViewConn(cfg.Requester, endpoint)
		if err != nil || c.endpoints[endpoint.Audience].conn != nil {
			if conn != nil {
				_ = conn.Close()
			}
			c.Close()
			return nil, ErrBrowserViewRejected
		}
		c.endpoints[endpoint.Audience] = browserViewEndpoint{conn: conn, client: runtimev1.NewBrowserSessionServiceClient(conn)}
	}
	return c, nil
}

func browserViewIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

// browserViewConn is the code owner client's transport: TLS 1.3, Main's one
// client certificate (whose identity must be requester), and a server that
// must present the audience's workload identity.
func browserViewConn(requester string, cfg BrowserViewEndpoint) (*grpc.ClientConn, error) {
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "" && u.Path != "/" || !browserViewIdentity(cfg.Audience) || cfg.TLS == nil || cfg.TLS.InsecureSkipVerify ||
		cfg.TLS.GetClientCertificate != nil || cfg.TLS.RootCAs == nil || len(cfg.TLS.Certificates) != 1 ||
		len(cfg.TLS.Certificates[0].Certificate) == 0 || cfg.TLS.Certificates[0].PrivateKey == nil {
		return nil, ErrBrowserViewRejected
	}
	leaf, err := x509.ParseCertificate(cfg.TLS.Certificates[0].Certificate[0])
	if err != nil {
		return nil, ErrBrowserViewRejected
	}
	if peer, err := workloadidentity.Certificate(leaf); err != nil || peer != requester {
		return nil, ErrBrowserViewRejected
	}
	tlsConfig := cfg.TLS.Clone()
	if tlsConfig.MinVersion < tls.VersionTLS13 {
		tlsConfig.MinVersion = tls.VersionTLS13
	}
	previous := tlsConfig.VerifyConnection
	audience := cfg.Audience
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return ErrBrowserViewRejected
		}
		if identity, err := workloadidentity.Certificate(state.PeerCertificates[0]); err != nil || identity != audience {
			return ErrBrowserViewRejected
		}
		if previous != nil {
			return previous(state)
		}
		return nil
	}
	return grpc.NewClient("dns:///"+u.Host,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithNoProxy(),
		grpc.WithDisableRetry(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(browserViewMessageBytes), grpc.MaxCallSendMsgSize(64<<10)),
	)
}

// Close closes every supervisor connection.
func (c *BrowserViewClient) Close() {
	if c == nil {
		return
	}
	for _, endpoint := range c.endpoints {
		_ = endpoint.conn.Close()
	}
}

// SignViewGrant signs the view grant for session: exported for its pinned
// test and for the relay's own tests.
func (c *BrowserViewClient) SignViewGrant(session browsersession.Session, control bool) (*runtimev1.SignedSandboxJobGrantV1, error) {
	if c == nil || !browsersession.ValidName(session.SessionID) || session.Spec.Name != session.SessionID ||
		browsersession.ValidateSpec(session.Spec) != nil || session.ExecutionID == "" || session.Generation == 0 ||
		session.Generation > math.MaxInt64 || !browserViewIdentity(session.TenantID) || session.ProjectID <= 0 || session.ProjectID > math.MaxInt32 {
		return nil, ErrBrowserViewRejected
	}
	if _, ok := c.endpoints[session.SupervisorAudience]; !ok {
		return nil, ErrBrowserViewRejected
	}
	digest := browsersession.ViewDigest(session.Spec, control)
	now := c.now().UTC()
	claims := &runtimev1.SandboxJobGrantClaimsV1{
		Revision: 1, TenantId: session.TenantID, ProjectId: int32(session.ProjectID), ExecutionId: session.ExecutionID, //nolint:gosec // bounded above
		ActivationId: session.SessionID, RequestDigest: digest[:], SubmitterWorkloadIdentity: c.requester,
		Audience: session.SupervisorAudience, IssuedAtUnixMillis: now.UnixMilli(),
		ExpiresAtUnixMillis: now.Add(browserViewGrantLifetime).UnixMilli(), Generation: session.Generation,
	}
	exact, err := proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil || len(exact) > 4096 {
		return nil, ErrBrowserViewRejected
	}
	input := make([]byte, len(browserViewGrantDomain)+8+len(exact))
	offset := copy(input, browserViewGrantDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(exact)))
	copy(input[offset+8:], exact)
	return &runtimev1.SignedSandboxJobGrantV1{KeyId: c.keyID, ClaimsBytes: exact, Signature: ed25519.Sign(c.key, input)}, nil
}

// browserViewErrorMetadata is the supervisor's typed refusal key
// (browser_session_service.rs ERROR_METADATA).
const browserViewErrorMetadata = "elitea-browser-error"

// BrowserViewError maps a supervisor refusal to the relay's kinds:
// browsersession.ErrViewSessionGone (no such session, or it ended),
// ErrViewBusy (a limit), ErrViewUnavailable (no answer, or not in time) and
// ErrViewRefused (the grant or the spec). typed is the refusal's
// elitea-browser-error value, "" for none. A nil err stays nil; an error
// that carries no gRPC status (a stream that ended) is ErrViewUnavailable.
func BrowserViewError(err error, typed string) error {
	if err == nil {
		return nil
	}
	switch typed {
	case "browser_session_not_started", "browser_session_ended":
		return browsersession.ErrViewSessionGone
	case "browser_capacity", "browser_session_limit":
		return browsersession.ErrViewBusy
	case "browser_session_fenced", "browser_agent_failed":
		return browsersession.ErrViewUnavailable
	}
	st, ok := grpcstatus.FromError(err)
	if !ok {
		return browsersession.ErrViewUnavailable
	}
	switch st.Code() {
	case codes.NotFound:
		return browsersession.ErrViewSessionGone
	case codes.ResourceExhausted:
		return browsersession.ErrViewBusy
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument, codes.FailedPrecondition:
		return browsersession.ErrViewRefused
	default:
		// Unavailable, DeadlineExceeded, Canceled, Internal, Unknown.
		return browsersession.ErrViewUnavailable
	}
}

// browserViewStream maps the stream's refusals: the supervisor refuses a
// view by ending the stream with a status, which the first Recv returns.
type browserViewStream struct {
	runtimev1.BrowserSessionService_BrowserViewClient
}

func (s browserViewStream) Recv() (*runtimev1.BrowserViewOutputV1, error) {
	out, err := s.BrowserSessionService_BrowserViewClient.Recv()
	if err != nil {
		typed := firstMetadata(s.Trailer(), browserViewErrorMetadata)
		if typed == "" {
			// A trailers-only refusal carries it in the one metadata block.
			if header, headerErr := s.Header(); headerErr == nil {
				typed = firstMetadata(header, browserViewErrorMetadata)
			}
		}
		return nil, BrowserViewError(err, typed)
	}
	return out, nil
}

func firstMetadata(md metadata.MD, key string) string {
	if values := md.Get(key); len(values) > 0 {
		return values[0]
	}
	return ""
}

// BrowserViewStream is one open view: the relay's half of the stream.
type BrowserViewStream interface {
	Send(*runtimev1.BrowserViewInputV1) error
	Recv() (*runtimev1.BrowserViewOutputV1, error)
	CloseSend() error
}

// OpenView opens a BrowserView stream to the supervisor the session row
// names and sends its open frame. The stream lives as long as ctx. A
// refusal arrives on the first Recv, mapped by BrowserViewError.
func (c *BrowserViewClient) OpenView(ctx context.Context, session browsersession.Session, control bool) (BrowserViewStream, error) {
	grant, err := c.SignViewGrant(session, control)
	if err != nil {
		return nil, err
	}
	endpoint := c.endpoints[session.SupervisorAudience]
	stream, err := endpoint.client.BrowserView(ctx)
	if err != nil {
		return nil, BrowserViewError(err, "")
	}
	spec := session.Spec
	open := &runtimev1.BrowserViewInputV1{Frame: &runtimev1.BrowserViewInputV1_Open{Open: &runtimev1.BrowserViewOpenV1{
		Grant: grant,
		Spec: &runtimev1.BrowserSessionSpecV1{
			Revision: spec.Revision, Name: spec.Name, Engine: runtimev1.BrowserEngineV1(spec.Engine),
			CpuMillis: spec.CPUMillis, MemoryBytes: spec.MemoryBytes, WallSeconds: spec.WallSeconds, MaxPages: spec.MaxPages,
		},
		Control: control,
		MaxFps:  c.maxFPS,
	}}}
	if err := stream.Send(open); err != nil {
		_ = stream.CloseSend()
		// io.EOF here means the server already ended the stream; its
		// status is on Recv.
		_, recvErr := browserViewStream{stream}.Recv()
		if recvErr == nil {
			recvErr = browsersession.ErrViewUnavailable
		}
		return nil, recvErr
	}
	return browserViewStream{stream}, nil
}
