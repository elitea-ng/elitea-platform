// Package vectorclient is elitea-main's client of elitea-vector
// (elitea.vector.v1.VectorService, libs/proto/elitea/vector/v1): the two
// deletions ADR-0031 decision 4 assigns to the platform itself. "A toolkit
// index: deleting it calls Delete on its namespace" is DeleteIndexVectors,
// run by the index delete route and again by the registry's tombstone sweeper
// for every tombstone it still holds. "A project: deleting it calls
// DropProject, which removes every source" is DeleteProjectData, one step of
// the project delete's cleanup journal.
//
// THE TRANSPORT IS gRPC OVER MUTUAL TLS, as AGENTS.md prescribes for internal
// service-to-service calls. elitea-vector authorises this process by ONE
// thing: the verified client certificate, whose identity (its one DNS SAN,
// read as `dns:<name>`, or its one SPIFFE URI SAN) must be in
// ELITEA_VECTOR_ADMIN_IDENTITIES. An administrator caller sends no token and
// may call Delete, DropProject, Count and ListIndexes only; it must name the
// project in every request. The certificate chains to the RUNTIME CA, the
// trust root of elitea-vector's listener (the same CA that signs the workers'
// identities and elitea-main's own control listener), not to the provider
// hops' CA that the DeepWiki and Inventory clients present.
//
// THE CALLS ARE IDEMPOTENT ON THE SERVER'S SIDE: deleting a namespace that
// holds no points, or a project that was never written, answers OK with the
// number of collections the filter was applied to. A caller that repeats a
// deletion it is not sure of (the sweeper, a journal retry) loses nothing.
//
// THE ERRORS ARE CLASSIFIED FOR THE CALLERS. The index deleter's contract
// (indexingapp.IndexVectorDeleter) is nil or not: any failure keeps the
// tombstone and the sweeper backs it off, so DeleteIndexVectors returns the
// gRPC status wrapped, with the code in the message. The project step reads
// platformops's classes, because it is the SAME cleanup journal that runs the
// engine deletions (projectprovisioning/application_data.go), so
// DeleteProjectData returns platformops's sentinels:
//
//   - platformops.ErrRetryLater: elitea-vector or Qdrant is unreachable or
//     slow (UNAVAILABLE, DEADLINE_EXCEEDED), or a precondition failed
//     (FAILED_PRECONDITION, ABORTED, RESOURCE_EXHAUSTED). The journal retries.
//   - platformops.ErrNotAllowed: elitea-vector refused this certificate
//     (UNAUTHENTICATED, PERMISSION_DENIED). A retry cannot fix it: the
//     allowlist or the material is wrong, and the message says so. The
//     journal closes the step with NoteRefused and an Error log.
//   - platformops.ErrConfig: the request is invalid (INVALID_ARGUMENT), which
//     for a fixed request shape means the client is wrong. Closed with
//     NoteInvalidRequest.
//   - anything else: elitea-vector failed (INTERNAL and the rest); the journal
//     retries it.
package vectorclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// App is the name the project delete's cleanup journal records the vector
// store under: its step is `vector_data`
// (projectprovisioning.StepVectorData), beside `deepwiki_data` and
// `inventory_data`.
const App = "vector"

const (
	// DefaultIndexDeleteTimeout bounds one Delete. The index delete route
	// calls it inside a user's request, so it is short; a timeout leaves the
	// tombstone, which the sweeper finishes.
	DefaultIndexDeleteTimeout = 30 * time.Second
	// DefaultProjectDropTimeout bounds one DropProject, which touches every
	// collection. Well below the cleanup journal's run bound (5 minutes), and
	// the step bounds the call further to its share of the run.
	DefaultProjectDropTimeout = 2 * time.Minute
)

// ErrConfig reports a Config that cannot build a client.
var ErrConfig = errors.New("vectorclient: invalid configuration")

// Config is what a client needs: where elitea-vector's listener is and the
// mutual-TLS material this process presents to it.
type Config struct {
	// Target is the listener's host:port (compose: elitea-vector:9470; the
	// chart derives <vector.service.name>:<vector.service.port>). No scheme:
	// the transport is always TLS.
	Target string
	// The client certificate this process presents (one DNS SAN, admitted by
	// elitea-vector as dns:<name>), its key, and the CA that signed
	// elitea-vector's server certificate: the runtime CA.
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	// ServerName is the name verified against elitea-vector's certificate.
	// Empty is the host of Target.
	ServerName string
	// IndexDeleteTimeout bounds one Delete; zero is DefaultIndexDeleteTimeout.
	IndexDeleteTimeout time.Duration
	// ProjectDropTimeout bounds one DropProject; zero is
	// DefaultProjectDropTimeout.
	ProjectDropTimeout time.Duration
}

// Validate reports the first thing that makes the configuration unusable.
func (c Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Target) == "":
		return fmt.Errorf("%w: the elitea-vector gRPC target is empty", ErrConfig)
	case strings.Contains(c.Target, "://"):
		return fmt.Errorf("%w: the elitea-vector gRPC target %q must be host:port, not a URL", ErrConfig, c.Target)
	}
	host, port, err := net.SplitHostPort(c.Target)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("%w: the elitea-vector gRPC target %q must be host:port", ErrConfig, c.Target)
	}
	for _, file := range []struct{ name, value string }{
		{"client certificate", c.ClientCertFile},
		{"client key", c.ClientKeyFile},
		{"CA bundle", c.CAFile},
	} {
		if strings.TrimSpace(file.value) == "" {
			return fmt.Errorf("%w: the elitea-vector gRPC client needs a %s file", ErrConfig, file.name)
		}
	}
	if c.IndexDeleteTimeout < 0 || c.ProjectDropTimeout < 0 {
		return fmt.Errorf("%w: a call timeout cannot be negative", ErrConfig)
	}
	return nil
}

// Client calls elitea-vector as the platform's administrator identity.
type Client struct {
	target       string
	conn         *grpc.ClientConn
	vectors      vectorv1.VectorServiceClient
	indexTimeout time.Duration
	dropTimeout  time.Duration
	logger       *slog.Logger
}

// New builds a client. The material is loaded now (a missing file is a
// construction error, not a per-call one); the connection is dialled lazily on
// the first call, which is grpc.NewClient's behaviour, so elitea-main does not
// wait for elitea-vector at boot (elitea-vector itself waits for elitea-main's
// control listener to verify tokens).
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	tlsConfig, err := mtlsConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: elitea-vector gRPC client: %w", ErrConfig, err)
	}
	conn, err := grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		// A response is a collection count; nothing legitimate is larger.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20)),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: elitea-vector gRPC client: %w", ErrConfig, err)
	}
	indexTimeout := cfg.IndexDeleteTimeout
	if indexTimeout == 0 {
		indexTimeout = DefaultIndexDeleteTimeout
	}
	dropTimeout := cfg.ProjectDropTimeout
	if dropTimeout == 0 {
		dropTimeout = DefaultProjectDropTimeout
	}
	return &Client{
		target: cfg.Target, conn: conn, vectors: vectorv1.NewVectorServiceClient(conn),
		indexTimeout: indexTimeout, dropTimeout: dropTimeout,
		logger: logger.With("target", cfg.Target),
	}, nil
}

// mtlsConfig is platformops's TLS configuration shape: the client
// certificate, the CA, the server-name rule, TLS 1.2 at least.
func mtlsConfig(cfg Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read ca bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from ca bundle %q", cfg.CAFile)
	}
	serverName := cfg.ServerName
	if serverName == "" {
		serverName, _, _ = net.SplitHostPort(cfg.Target)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// Target is the listener this client dials.
func (c *Client) Target() string { return c.target }

// App is the journal name of the vector store (App). It lets a *Client stand
// where the composition lists application-data clients.
func (c *Client) App() string { return App }

// Close releases the connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// indexIDPattern is the canonical lower-case UUID form elitea-vector requires
// of a namespace_id. The registry writes its index ids in that form; anything
// else never names a namespace and is refused here, before the wire.
var indexIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// DeleteIndexVectors implements indexingapp.IndexVectorDeleter: Delete of the
// whole namespace of one toolkit index (source TOOLKIT_INDEX, namespace_id the
// index_registry UUID), in every collection. Idempotent: a namespace that holds
// nothing is a clean success. Any error means the vectors may still be there,
// and the caller keeps its tombstone.
func (c *Client) DeleteIndexVectors(ctx context.Context, namespace indexingapp.IndexVectorNamespace) error {
	if c == nil {
		return fmt.Errorf("%w: no elitea-vector client", ErrConfig)
	}
	if namespace.ProjectID <= 0 {
		return fmt.Errorf("%w: project id %d is not positive", ErrConfig, namespace.ProjectID)
	}
	if !indexIDPattern.MatchString(namespace.IndexID) {
		return fmt.Errorf("%w: index id %q is not a canonical uuid", ErrConfig, namespace.IndexID)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.indexTimeout)
	defer cancel()
	response, err := c.vectors.Delete(callCtx, &vectorv1.DeleteRequest{
		ProjectId: int64(namespace.ProjectID),
		Namespace: &vectorv1.Namespace{
			Source:      vectorv1.Source_SOURCE_TOOLKIT_INDEX,
			NamespaceId: namespace.IndexID,
		},
		Selector: &vectorv1.DeleteRequest_WholeNamespace{WholeNamespace: &vectorv1.WholeNamespace{}},
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The caller's context ended (a shutdown, a request cut off): the
			// sweeper tells that apart from a failure of the store.
			return ctxErr
		}
		return c.classifyIndexDelete(namespace, err)
	}
	c.logger.Info("index vectors deleted", "project_id", namespace.ProjectID,
		"index_id", namespace.IndexID, "collections", response.GetCollections())
	return nil
}

// Deferred is false: this deleter deletes. The tombstone sweeper runs.
func (c *Client) Deferred() bool { return false }

// classifyIndexDelete wraps a failed Delete. A refused certificate is named
// as such, because no retry fixes it and the sweeper would otherwise back the
// tombstone off for ever with a message that reads like an outage.
func (c *Client) classifyIndexDelete(namespace indexingapp.IndexVectorNamespace, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%w: elitea-vector delete of index %s (project %d): %s (%s)",
			platformops.ErrNotAllowed, namespace.IndexID, namespace.ProjectID, message, code)
	}
	return fmt.Errorf("elitea-vector delete of index %s (project %d) failed: %s (%s)",
		namespace.IndexID, namespace.ProjectID, message, code)
}

// DeleteProjectData implements projectprovisioning.ApplicationDataRemover:
// DropProject, which removes every point of the project in every collection
// and for every source (toolkit indexes, wikis). Idempotent on the server's
// side. The summary names the collections the drop was applied to; the errors
// are platformops's classes (see the package comment).
func (c *Client) DeleteProjectData(ctx context.Context, projectID int64) (string, error) {
	if c == nil {
		return "", fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if projectID <= 0 || projectID > 1<<31-1 {
		return "", fmt.Errorf("%w: project id %d is not a positive int32", platformops.ErrConfig, projectID)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.dropTimeout)
	defer cancel()
	response, err := c.vectors.DropProject(callCtx, &vectorv1.DropProjectRequest{ProjectId: projectID})
	if err != nil {
		return "", c.classifyProjectDrop(projectID, err)
	}
	summary := fmt.Sprintf("dropped the project's vectors in %d collection(s)", response.GetCollections())
	c.logger.Info("project vectors dropped", "project_id", projectID, "collections", response.GetCollections())
	return summary, nil
}

// classifyProjectDrop turns a failed DropProject into one of platformops's
// errors (see the package comment). The status message is kept: elitea-vector
// writes it without statements or credentials.
func (c *Client) classifyProjectDrop(projectID int64, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.FailedPrecondition, codes.Aborted, codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return fmt.Errorf("%w: elitea-vector drop_project %d: %s (%s)", platformops.ErrRetryLater, projectID, message, code)
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%w: elitea-vector drop_project %d: %s (%s). Its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's client certificate identity",
			platformops.ErrNotAllowed, projectID, message, code)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: elitea-vector drop_project %d: %s (%s)", platformops.ErrConfig, projectID, message, code)
	case codes.Canceled:
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("elitea-vector drop_project %d: %s (%s)", projectID, message, code)
	}
	return fmt.Errorf("elitea-vector drop_project %d failed: %s (%s)", projectID, message, code)
}

var _ indexingapp.IndexVectorDeleter = (*Client)(nil)
