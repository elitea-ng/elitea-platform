// Package artifacts is the host's client for the platform's artifact objects:
// elitea.runtime.v1.ArtifactObjectService on elitea-main's platform gRPC
// listener (#1359 U5), over mTLS with the sub-application callback identity
// and the bearer the facade minted for the invocation.
//
// It was DeepWiki's (internal/apps/deepwiki/run/artifacts.go) until ADR-0023
// stage H4c, and nothing in it is DeepWiki's: the service, the bearer and the
// refusal texts are the platform's, and any sub-application that produces
// artifact objects stores them exactly this way. What stayed behind in each
// application's package is the part that is not generic — reading the bearer
// and the project out of the legacy llm_settings dict, and deciding when there
// is no transport at all.
//
// Until #1359 U5 this was an HTTP callback into elitea-main's artifact routes
// at an origin the facade put in llm_settings.api_base; that origin is now a
// route name only, and the address is the host's own
// <PREFIX>ARTIFACT_SERVICE_URL.
package artifacts

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// Client puts one object into a bucket. The real one is the platform's
// artifact service; tests hand in a fake.
type Client interface {
	Upload(ctx context.Context, bucket, name string, data []byte) error
}

// Factory builds the client for one invocation from its llm_settings — the
// bearer the facade minted and the project it is bound to. nil means "no
// transport", and nothing uploads.
type Factory func(llmSettings map[string]any) (Client, error)

// Settings is what one invocation's llm_settings carry for artifacts: the
// minted bearer and the project the buckets belong to.
type Settings struct {
	APIKey    string
	ProjectID string
}

// Callback is the hop to the platform's artifact service: its address
// (<PREFIX>ARTIFACT_SERVICE_URL, `https://elitea-main:9446` or host:port), the
// bundle it trusts, and the client certificate it presents — the platform
// listener requires one signed by the runtime CA at the handshake. TargetEnv
// names the variable, for the refusal when it is unset.
type Callback struct {
	Target    string
	TargetEnv string
	CAFile    string
	CertFile  string
	KeyFile   string
}

// RefusalTrailer is the trailer elitea-main's artifact service puts a
// runtimev1.ArtifactRefusalV1 in (artifactgrpc.RefusalTrailer).
const RefusalTrailer = "elitea-artifact-refusal-bin"

// chunkBytes is the largest data frame Upload sends.
const chunkBytes = 1 << 20

// maxMessageBytes is the platform listener's own limit
// (runtimegrpc.PlatformMaxMessageBytes).
const maxMessageBytes = 16 << 20

// Dial opens the channel to the artifact service. It is lazy — no connection
// is made until the first call — so a host whose invocations never carry a
// transport never dials.
func Dial(callback Callback) (*grpc.ClientConn, error) {
	if strings.TrimSpace(callback.Target) == "" {
		name := callback.TargetEnv
		if name == "" {
			name = "the artifact service address"
		}
		return nil, fmt.Errorf("%s is not set: artifacts go to elitea.runtime.v1.ArtifactObjectService on the platform listener", name)
	}
	if (callback.CertFile == "") != (callback.KeyFile == "") {
		return nil, fmt.Errorf("the callback client certificate and key must be set together")
	}
	address, serverName, err := target(callback.Target)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, NextProtos: []string{"h2"}}
	if callback.CAFile != "" {
		pem, err := os.ReadFile(callback.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read the callback CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", callback.CAFile)
		}
		config.RootCAs = pool
	}
	if callback.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(callback.CertFile, callback.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load the callback client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{pair}
	}
	return grpc.NewClient("passthrough:///"+address,
		grpc.WithTransportCredentials(credentials.NewTLS(config)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes), grpc.MaxCallSendMsgSize(maxMessageBytes)))
}

// target accepts `https://host:port` or `host:port` and returns the dial
// address and the TLS server name.
func target(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.User != nil {
			return "", "", fmt.Errorf("the artifact service address %q must be https://host:port", raw)
		}
		raw = parsed.Host
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || port == "" {
		return "", "", fmt.Errorf("the artifact service address %q must name a host and a port", raw)
	}
	return raw, host, nil
}

// FactoryFor is the default factory over one shared, lazily opened channel:
// nil when llm_settings carry no bearer (a direct SPI call, a fixture), else
// a client bound to the invocation's bearer and project. The dial error, if
// any, is every invocation's error — the host was given no way to store.
func FactoryFor(callback Callback, extract func(map[string]any) Settings, hasTransport func(map[string]any) bool) Factory {
	var (
		once sync.Once
		conn *grpc.ClientConn
		err  error
	)
	return func(llmSettings map[string]any) (Client, error) {
		if !hasTransport(llmSettings) {
			return nil, nil
		}
		once.Do(func() { conn, err = Dial(callback) })
		if err != nil {
			return nil, err
		}
		return NewClient(conn, extract(llmSettings))
	}
}

// GRPCClient is one invocation's view of the artifact service.
type GRPCClient struct {
	Settings Settings
	project  int64
	service  runtimev1.ArtifactObjectServiceClient
}

// NewClient validates the settings the way the Python client did and binds
// them to conn.
func NewClient(conn grpc.ClientConnInterface, settings Settings) (*GRPCClient, error) {
	if settings.APIKey == "" {
		return nil, fmt.Errorf("artifact_settings.api_key is required")
	}
	project, err := strconv.ParseInt(strings.TrimSpace(settings.ProjectID), 10, 64)
	if err != nil || project <= 0 {
		return nil, fmt.Errorf("artifact_settings.project_id must be a positive integer, got %q", settings.ProjectID)
	}
	return &GRPCClient{Settings: settings, project: project, service: runtimev1.NewArtifactObjectServiceClient(conn)}, nil
}

func (c *GRPCClient) authorized(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.Settings.APIKey)
}

// Upload puts one object, overwriting an existing one. The object carries no
// media type, deliberately: elitea-main then derives it from the key's
// extension (.json → application/json, .md → text/markdown), which is what
// the wiki browser's manifest read depends on — a manifest stored as
// octet-stream is a wiki the browser cannot see.
//
// A 403 and every other refusal are errors carrying the platform's own text,
// as the Python client raised them; the runner reports them in band.
func (c *GRPCClient) Upload(ctx context.Context, bucket, name string, data []byte) error {
	ctx, cancel := context.WithCancel(c.authorized(ctx))
	defer cancel()
	stream, err := c.service.PutObject(ctx)
	if err == nil {
		err = stream.Send(&runtimev1.ArtifactPutObjectRequestV1{Frame: &runtimev1.ArtifactPutObjectRequestV1_Head{
			Head: &runtimev1.ArtifactPutObjectHeadV1{ProjectId: c.project, Bucket: strings.ToLower(bucket), Key: name, Overwrite: true},
		}})
	}
	for offset := 0; err == nil && offset < len(data); offset += chunkBytes {
		end := min(offset+chunkBytes, len(data))
		err = stream.Send(&runtimev1.ArtifactPutObjectRequestV1{Frame: &runtimev1.ArtifactPutObjectRequestV1_Data{Data: data[offset:end]}})
	}
	if err == nil || errors.Is(err, io.EOF) {
		_, err = stream.CloseAndRecv()
	}
	if err == nil {
		return nil
	}
	// The texts are the Python client's, capitals included: they surface
	// verbatim in the in-band "⚠️ … could not be uploaded" message.
	refusal := refusalOf(err, stream)
	switch {
	case refusal == nil:
		return fmt.Errorf("Failed to upload artifact: %v", err) //nolint:staticcheck // legacy message text
	case refusal.GetHttpStatus() == 403:
		return fmt.Errorf("Not authorized to upload artifact (HTTP 403): %s", clip(describe(refusal), 200)) //nolint:staticcheck // legacy message text
	default:
		return fmt.Errorf("Failed to upload artifact: HTTP %d — %s", refusal.GetHttpStatus(), clip(describe(refusal), 500)) //nolint:staticcheck // legacy message text
	}
}

// trailered is a stream whose trailer can be read after it ended.
type trailered interface{ Trailer() metadata.MD }

// refusalOf decodes the platform's refusal from a stream's trailer (or, for
// a unary call, from md). nil is a hop failure: no route answered.
func refusalOf(err error, stream trailered, md ...metadata.MD) *runtimev1.ArtifactRefusalV1 {
	if _, ok := status.FromError(err); !ok {
		return nil
	}
	var trailer metadata.MD
	if len(md) > 0 {
		trailer = md[0]
	} else if stream != nil {
		trailer = stream.Trailer()
	}
	values := trailer.Get(RefusalTrailer)
	if len(values) != 1 {
		return nil
	}
	refusal := &runtimev1.ArtifactRefusalV1{}
	if proto.Unmarshal([]byte(values[0]), refusal) != nil || refusal.GetHttpStatus() == 0 {
		return nil
	}
	return refusal
}

// describe is the refusal's text: "code: message", or whichever is present.
func describe(refusal *runtimev1.ArtifactRefusalV1) string {
	switch {
	case refusal.GetCode() != "" && refusal.GetMessage() != "":
		return refusal.GetCode() + ": " + refusal.GetMessage()
	case refusal.GetMessage() != "":
		return refusal.GetMessage()
	}
	return refusal.GetCode()
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
