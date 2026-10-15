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
//     (UNAUTHENTICATED, PERMISSION_DENIED): the allowlist or the material is
//     wrong, and the message says so. For THIS store the journal does NOT
//     close the step (that would leave the project's vectors in Qdrant for
//     ever behind a "done"): the vector step keeps it pending at the journal's
//     longest backoff with an Error log naming the identity problem, and the
//     deletion runs once an operator fixes the allowlist or the material.
//   - platformops.ErrConfig: the request is invalid (INVALID_ARGUMENT), which
//     for a fixed request shape means the client is wrong. Closed with
//     NoteInvalidRequest.
//   - anything else: elitea-vector failed (INTERNAL and the rest); the journal
//     retries it.
//
// The index deleter's refusal is indexingapp.ErrIndexVectorDeletionRefused
// (wrapped beside platformops.ErrNotAllowed), which the tombstone sweeper
// backs off to its longest pause at once rather than counting up to it.
//
// THE MATERIAL ROTATES: the certificate, key and CA are re-read when their
// files change (infra/mtlsclient, shared with the platform-operations hops),
// so a renewed Secret is used by the next connection without a restart.
package vectorclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mtlsclient"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// App is the name the project delete's cleanup journal records the vector
// store under: its step is `vector_data`
// (projectprovisioning.StepVectorData), beside `deepwiki_data` and
// `inventory_data`. The provisioner treats this application specially
// (projectprovisioning.AppVector), so the constant is its.
const App = projectprovisioning.AppVector

const (
	// DefaultIndexDeleteTimeout bounds one Delete as the SWEEPER calls it. The
	// index delete route bounds its own best-effort attempt far shorter
	// (indexregistry.RouteVectorDeleteBudget, through the context it passes),
	// and the smaller bound wins; a timeout leaves the tombstone, which the
	// sweeper finishes.
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
	// The material's own rules (the three files, a derivable server name) are
	// mtlsclient's: one definition for every mTLS hop this process makes.
	if err := c.material().Validate(); err != nil {
		return fmt.Errorf("%w: the elitea-vector gRPC client: %w", ErrConfig, err)
	}
	if c.IndexDeleteTimeout < 0 || c.ProjectDropTimeout < 0 {
		return fmt.Errorf("%w: a call timeout cannot be negative", ErrConfig)
	}
	return nil
}

// material is the mutual-TLS hop the configuration names, in mtlsclient's
// terms.
func (c Config) material() mtlsclient.Material {
	return mtlsclient.Material{
		CertFile: c.ClientCertFile, KeyFile: c.ClientKeyFile, CAFile: c.CAFile,
		ServerName: c.ServerName, Target: c.Target,
	}
}

// Client calls elitea-vector as the platform's administrator identity.
type Client struct {
	conn         *grpc.ClientConn
	vectors      vectorv1.VectorServiceClient
	indexTimeout time.Duration
	dropTimeout  time.Duration
	logger       *slog.Logger
}

// New builds a client. The material is loaded now (a missing file is a
// construction error, not a per-call one) and re-read when its files change
// (mtlsclient); the connection is dialled lazily on the first call, which is
// grpc.NewClient's behaviour, so elitea-main does not wait for elitea-vector
// at boot (elitea-vector itself waits for elitea-main's control listener to
// verify tokens).
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("target", cfg.Target)
	creds, err := mtlsclient.NewCredentials(cfg.material(), func(err error) {
		logger.Error("elitea-vector client material could not be reloaded", "err", err)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: elitea-vector gRPC client: %w", ErrConfig, err)
	}
	conn, err := grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(creds),
		// A response is a collection count; nothing legitimate is larger. The
		// calls FAIL FAST: a call made while the connection is down (the
		// route's best-effort attempt, a sweep during an outage) returns
		// UNAVAILABLE at once instead of waiting out its deadline for the
		// store to come back.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20), grpc.WaitForReady(false)),
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
		conn: conn, vectors: vectorv1.NewVectorServiceClient(conn),
		indexTimeout: indexTimeout, dropTimeout: dropTimeout,
		logger: logger,
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

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
	// The canonical lower-case UUID form elitea-vector requires of a
	// namespace_id; the registry writes its ids in that form, so anything
	// else never names a namespace and is refused before the wire.
	if !indexingapp.ValidCanonicalUUID(namespace.IndexID) {
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
		if ctxErr := callerEnded(ctx); ctxErr != nil {
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
// as such (indexingapp.ErrIndexVectorDeletionRefused and
// platformops.ErrNotAllowed), because only an operator fixes it: the sweeper
// then waits its longest pause between attempts instead of polling a store
// that refuses it, and the message says what to fix rather than reading like
// an outage.
func (c *Client) classifyIndexDelete(namespace indexingapp.IndexVectorNamespace, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.Unauthenticated, codes.PermissionDenied:
		c.logger.Error("elitea-vector refused this process's client certificate; its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's identity, and the deletion waits until it does",
			"project_id", namespace.ProjectID, "index_id", namespace.IndexID, "code", code.String(), "message", message)
		return fmt.Errorf("%w: %w: elitea-vector delete of index %s (project %d): %s (%s)",
			indexingapp.ErrIndexVectorDeletionRefused, platformops.ErrNotAllowed,
			namespace.IndexID, namespace.ProjectID, message, code)
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
		if ctxErr := callerEnded(ctx); ctxErr != nil {
			// The caller's context ended (a shutdown, the run's bound): the
			// journal tells that apart from a failure of the store.
			return "", ctxErr
		}
		return "", c.classifyProjectDrop(projectID, err)
	}
	summary := fmt.Sprintf("dropped the project's vectors in %d collection(s)", response.GetCollections())
	c.logger.Info("project vectors dropped", "project_id", projectID, "collections", response.GetCollections())
	return summary, nil
}

// classifyProjectDrop turns a failed DropProject into one of platformops's
// errors (see the package comment). The status message is kept: elitea-vector
// writes it without statements or credentials. The caller's own cancellation
// never reaches here (DeleteProjectData returns ctx.Err() first).
func (c *Client) classifyProjectDrop(projectID int64, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.FailedPrecondition, codes.Aborted, codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return fmt.Errorf("%w: elitea-vector drop_project %d: %s (%s)", platformops.ErrRetryLater, projectID, message, code)
	case codes.Unauthenticated, codes.PermissionDenied:
		c.logger.Error("elitea-vector refused this process's client certificate; its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's identity, and the project's vectors wait in Qdrant until it does",
			"project_id", projectID, "code", code.String(), "message", message)
		return fmt.Errorf("%w: elitea-vector drop_project %d: %s (%s). Its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's client certificate identity",
			platformops.ErrNotAllowed, projectID, message, code)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: elitea-vector drop_project %d: %s (%s)", platformops.ErrConfig, projectID, message, code)
	}
	return fmt.Errorf("elitea-vector drop_project %d failed: %s (%s)", projectID, message, code)
}

// DeleteMemoryToolkit removes every memory point of one memory toolkit of a
// project, whoever owns it (the project's shared memories and every user's
// personal ones) and whatever namespace label it used, in every collection. It
// is the toolkit cleanup journal's call when a memory toolkit is deleted.
// Idempotent. The errors are platformops's classes, like DeleteProjectData.
func (c *Client) DeleteMemoryToolkit(ctx context.Context, projectID, toolkitID int64) error {
	if c == nil {
		return fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if projectID <= 0 || projectID > 1<<31-1 || toolkitID <= 0 {
		return fmt.Errorf("%w: project %d and toolkit %d must be positive", platformops.ErrConfig, projectID, toolkitID)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.indexTimeout)
	defer cancel()
	response, err := c.vectors.DeleteMemoryToolkit(callCtx, &vectorv1.DeleteMemoryToolkitRequest{
		ProjectId: projectID,
		ToolkitId: toolkitID,
	})
	if err != nil {
		if ctxErr := callerEnded(ctx); ctxErr != nil {
			return ctxErr
		}
		return c.classifyMemoryDelete("delete_memory_toolkit", projectID, err)
	}
	c.logger.Info("memory toolkit deleted", "project_id", projectID, "toolkit_id", toolkitID, "collections", response.GetCollections())
	return nil
}

// DeleteMemoryUser removes every PERSONAL memory point of one user in one
// project, or (projectID 0) in every project, in every collection: what a
// user's removal from a project (or the user's deletion) owes. Idempotent. The
// errors are platformops's classes.
func (c *Client) DeleteMemoryUser(ctx context.Context, projectID, userID int64) error {
	if c == nil {
		return fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if projectID < 0 || projectID > 1<<31-1 || userID <= 0 {
		return fmt.Errorf("%w: project %d must not be negative and user %d must be positive", platformops.ErrConfig, projectID, userID)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.indexTimeout)
	defer cancel()
	response, err := c.vectors.DeleteMemoryUser(callCtx, &vectorv1.DeleteMemoryUserRequest{
		ProjectId: projectID,
		UserId:    userID,
	})
	if err != nil {
		if ctxErr := callerEnded(ctx); ctxErr != nil {
			return ctxErr
		}
		return c.classifyMemoryDelete("delete_memory_user", projectID, err)
	}
	c.logger.Info("a user's personal memories deleted", "project_id", projectID, "user_id", userID, "collections", response.GetCollections())
	return nil
}

// classifyMemoryDelete turns a failed memory delete into one of platformops's
// errors, as classifyProjectDrop does; a refused certificate is logged with
// what to fix.
func (c *Client) classifyMemoryDelete(operation string, projectID int64, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.FailedPrecondition, codes.Aborted, codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return fmt.Errorf("%w: elitea-vector %s (project %d): %s (%s)", platformops.ErrRetryLater, operation, projectID, message, code)
	case codes.Unauthenticated, codes.PermissionDenied:
		c.logger.Error("elitea-vector refused this process's client certificate; its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's identity, and the memories wait until it does",
			"project_id", projectID, "operation", operation, "code", code.String(), "message", message)
		return fmt.Errorf("%w: elitea-vector %s (project %d): %s (%s). Its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's client certificate identity",
			platformops.ErrNotAllowed, operation, projectID, message, code)
	case codes.Unimplemented:
		return fmt.Errorf("%w: elitea-vector %s", platformops.ErrNotSupported, operation)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: elitea-vector %s (project %d): %s (%s)", platformops.ErrConfig, operation, projectID, message, code)
	}
	return fmt.Errorf("elitea-vector %s (project %d) failed: %s (%s)", operation, projectID, message, code)
}

var _ indexingapp.IndexVectorDeleter = (*Client)(nil)

// callerEnded reports the caller's context ending: its error, or
// context.DeadlineExceeded once its deadline has passed. gRPC enforces the
// same deadline with its own timer, so a call can fail with DeadlineExceeded
// a moment before ctx.Err() turns non-nil.
func callerEnded(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
