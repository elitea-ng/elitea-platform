// Package platformops is elitea-main's client of a sub-application's platform
// service (elitea.subapp.v1.PlatformOperations, libs/proto): the operations the
// platform itself runs against a DeepWiki or Inventory engine, as opposed to
// the toolkit tools users reach through the facade. Today those are the two
// deletions of issues #1243 and #1244: DeleteProject when a project is
// deprovisioned, DeleteToolkit when a toolkit of the application is deleted.
//
// THE TRANSPORT IS gRPC OVER MUTUAL TLS, as AGENTS.md prescribes for internal
// service-to-service calls, and the engine authorises a call by ONE thing: the
// verified client certificate of this process, matched against its allowlist
// (ELITEA_<APP>_PLATFORM_CLIENTS). This client therefore presents the SAME
// client certificate the facade's HTTPS hop presents (ELITEA_<APP>_CLIENT_
// CERT_FILE / _KEY_FILE), trusts the SAME CA, and verifies the SAME server
// name: the engine serves both listeners behind one TLS configuration, so
// one set of material covers both hops and nothing new has to be issued.
//
// THE ERRORS ARE CLASSIFIED FOR THE CALLERS, which are a cleanup journal and a
// delete route and neither reads gRPC codes:
//
//   - ErrRetryLater: nothing (or not everything) was deleted and calling again
//     later is the right thing: the engine is still stopping a generation or an
//     ingest (FAILED_PRECONDITION), a publish holds the wiki (ABORTED), the
//     engine is unreachable or slow (UNAVAILABLE, DEADLINE_EXCEEDED), or a
//     project deletion answered OK with `incomplete` (what it deleted stays
//     deleted; the rest is held by a run that started meanwhile).
//   - ErrNotSupported: the application does not implement the operation
//     (UNIMPLEMENTED; DeepWiki keeps nothing per toolkit, its index is per
//     wiki). The cleanup journals close the step as "unsupported"; a toolkit
//     delete never asks DeepWiki in the first place (AppKeepsToolkitData).
//   - ErrNotAllowed: the engine refused this process's certificate
//     (UNAUTHENTICATED, PERMISSION_DENIED). A retry cannot fix it; the
//     allowlist or the material is wrong, and the message says so.
//   - anything else: the engine failed (INTERNAL and the rest). The journal
//     retries it, the route reports it.
package platformops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mtlsclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The two applications that serve the platform service today. App names are
// the names the facades and the composition know them by; the toolkit types
// each publishes are pinned in toolkitTypeApps (see AppForToolkitType).
const (
	AppDeepWiki  = "deepwiki"
	AppInventory = "inventory"
)

var (
	// ErrRetryLater: nothing, or not everything, was deleted, and a later call
	// may succeed. See the package comment.
	ErrRetryLater = errors.New("platformops: the engine could not delete everything now; retry")
	// ErrNotSupported: the application has nothing of this kind to delete.
	ErrNotSupported = errors.New("platformops: the application does not support this operation")
	// ErrNotAllowed: the engine refused this process's client certificate. The
	// engine's ELITEA_<APP>_PLATFORM_CLIENTS allowlist must name the identity
	// (subject CN or DNS SAN) of the certificate ELITEA_<APP>_CLIENT_CERT_FILE
	// holds.
	ErrNotAllowed = errors.New("platformops: the engine does not allow this client certificate to call platform operations")
	// ErrConfig reports a Config that cannot build a client.
	ErrConfig = errors.New("platformops: invalid configuration")
)

// DefaultCallTimeout bounds one call. The engine stops the project's running
// generations or ingests first and waits a bounded time for them (30s by
// default on the host), so the bound is well above that and well below the
// cleanup journal's run bound (5 minutes).
const DefaultCallTimeout = 2 * time.Minute

// Config is what a client needs: where the engine's platform listener is and
// the mutual-TLS material of the facade hop to the same engine.
type Config struct {
	// App is the application the engine serves: AppDeepWiki or AppInventory.
	App string
	// Target is the listener's host:port (e.g. elitea-deepwiki:9443). No
	// scheme: the transport is always TLS.
	Target string
	// The client certificate this process presents, its key, and the CA that
	// signed the engine's server certificate. The same three files as the
	// facade's ELITEA_<APP>_CLIENT_CERT_FILE / _CLIENT_KEY_FILE / _CA_FILE.
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	// ServerName is the name verified against the engine's certificate. Empty
	// is the host of Target, which is what the facade does with its base URL.
	ServerName string
	// CallTimeout bounds one call; zero is DefaultCallTimeout.
	CallTimeout time.Duration
}

// Validate reports the first thing that makes the configuration unusable.
func (c Config) Validate() error {
	switch {
	case c.App != AppDeepWiki && c.App != AppInventory:
		return fmt.Errorf("%w: unknown application %q", ErrConfig, c.App)
	case strings.TrimSpace(c.Target) == "":
		return fmt.Errorf("%w: the %s platform gRPC target is empty", ErrConfig, c.App)
	case strings.Contains(c.Target, "://"):
		return fmt.Errorf("%w: the %s platform gRPC target %q must be host:port, not a URL", ErrConfig, c.App, c.Target)
	}
	host, port, err := net.SplitHostPort(c.Target)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("%w: the %s platform gRPC target %q must be host:port", ErrConfig, c.App, c.Target)
	}
	// The material's own rules (the three files, a derivable server name) are
	// mtlsclient's: one definition for every mTLS hop this process makes.
	if err := c.material().Validate(); err != nil {
		return fmt.Errorf("%w: the %s platform gRPC client: %w", ErrConfig, c.App, err)
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

// Client calls one application's platform service.
type Client struct {
	app     string
	target  string
	conn    *grpc.ClientConn
	ops     subappv1.PlatformOperationsClient
	timeout time.Duration
	logger  *slog.Logger
}

// New builds a client. The material is loaded now (a missing file is a
// construction error, not a per-call one) and re-read when its files change
// (infra/mtlsclient: the facade hop's shape — the same client certificate, the
// same CA, the same server-name rule, h2 — shared with the elitea-vector
// client, so a renewed Secret is used by the next connection without a
// restart); the connection is dialled lazily on the first call, which is
// grpc.NewClient's behaviour.
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	hopLogger := logger.With("app", cfg.App, "target", cfg.Target)
	creds, err := mtlsclient.NewCredentials(cfg.material(), func(err error) {
		hopLogger.Error("platform gRPC client material could not be reloaded", "err", err)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s platform gRPC client: %w", ErrConfig, cfg.App, err)
	}
	conn, err := grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(creds),
		// A response is counts and ids; nothing legitimate is larger.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20)),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s platform gRPC client: %w", ErrConfig, cfg.App, err)
	}
	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	return &Client{
		app: cfg.App, target: cfg.Target, conn: conn,
		ops: subappv1.NewPlatformOperationsClient(conn), timeout: timeout,
		logger: hopLogger,
	}, nil
}

// App is the application this client speaks to.
func (c *Client) App() string { return c.app }

// Target is the listener this client dials.
func (c *Client) Target() string { return c.target }

// Close releases the connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// ProjectDeletion is what DeleteProject removed, as the step note reports it.
type ProjectDeletion struct {
	Wikis         int
	GraphToolkits int
	LiveBuilds    int64
	Incomplete    bool
	BusyToolkits  []int64
	Errors        []string
}

// Summary is a one-line account for a journal note or a log.
func (d ProjectDeletion) Summary() string {
	parts := []string{}
	if d.Wikis > 0 {
		parts = append(parts, fmt.Sprintf("%d wiki index(es)", d.Wikis))
	}
	if d.GraphToolkits > 0 {
		parts = append(parts, fmt.Sprintf("%d graph(s)", d.GraphToolkits))
	}
	if len(parts) == 0 {
		return "nothing to delete"
	}
	return "deleted " + strings.Join(parts, ", ")
}

// DeleteProjectData removes everything the application holds for the project
// (the search index of every wiki, or every graph). It is idempotent on the
// engine's side. It returns the summary of a COMPLETE deletion, or an error:
// ErrRetryLater when the engine deleted nothing yet (a run is still stopping)
// or not everything (`incomplete`), ErrNotAllowed when the certificate was
// refused, and the engine's failure otherwise. The summary is also returned
// with an ErrRetryLater for an incomplete deletion, so a caller can record
// what went.
func (c *Client) DeleteProjectData(ctx context.Context, projectID int64) (string, error) {
	if projectID <= 0 || projectID > 1<<31-1 {
		return "", fmt.Errorf("%w: project id %d is not a positive int32", ErrConfig, projectID)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.ops.DeleteProject(callCtx, &subappv1.DeleteProjectRequest{ProjectId: int32(projectID)})
	if err != nil {
		return "", c.classify("delete_project", err)
	}
	deletion := ProjectDeletion{
		Wikis:         len(response.GetWikis()),
		GraphToolkits: len(response.GetGraphToolkitIds()),
		LiveBuilds:    response.GetLiveBuilds(),
		Incomplete:    response.GetIncomplete(),
		BusyToolkits:  response.GetBusyToolkitIds(),
		Errors:        response.GetErrors(),
	}
	for _, line := range deletion.Errors {
		c.logger.Warn("platform delete_project reported a problem that did not stop it",
			"project_id", projectID, "problem", line)
	}
	if deletion.Incomplete {
		return deletion.Summary(), fmt.Errorf("%w: %s project %d: %s; still held: %d busy toolkit(s), %d live build(s)",
			ErrRetryLater, c.app, projectID, deletion.Summary(), len(deletion.BusyToolkits), deletion.LiveBuilds)
	}
	return deletion.Summary(), nil
}

// ToolkitDeletion is what DeleteToolkit removed.
type ToolkitDeletion struct {
	// Deleted is false when the toolkit had nothing in the engine.
	Deleted bool
}

// DeleteToolkitData removes what the application holds for one toolkit of the
// project (Inventory: its graph). ErrNotSupported is the application's answer
// when it keeps nothing per toolkit (DeepWiki); the other errors are as for
// DeleteProjectData.
func (c *Client) DeleteToolkitData(ctx context.Context, projectID, toolkitID int64) (ToolkitDeletion, error) {
	if projectID <= 0 || projectID > 1<<31-1 {
		return ToolkitDeletion{}, fmt.Errorf("%w: project id %d is not a positive int32", ErrConfig, projectID)
	}
	if toolkitID <= 0 || toolkitID > 1<<31-1 {
		return ToolkitDeletion{}, fmt.Errorf("%w: toolkit id %d is not a positive int32", ErrConfig, toolkitID)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.ops.DeleteToolkit(callCtx, &subappv1.DeleteToolkitRequest{
		ProjectId: int32(projectID), ToolkitId: int32(toolkitID),
	})
	if err != nil {
		return ToolkitDeletion{}, c.classify("delete_toolkit", err)
	}
	return ToolkitDeletion{Deleted: response.GetDeleted()}, nil
}

// classify turns a gRPC failure into one of the package's errors (see the
// package comment). The status message is kept: the engine writes it without
// statements or credentials.
func (c *Client) classify(op string, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.FailedPrecondition, codes.Aborted, codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s %s: %s (%s)", ErrRetryLater, c.app, op, message, code)
	case codes.Unimplemented:
		return fmt.Errorf("%w: %s %s", ErrNotSupported, c.app, op)
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%w: %s %s: %s (%s)", ErrNotAllowed, c.app, op, message, code)
	case codes.Canceled:
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("%s %s: %s (%s)", c.app, op, message, code)
	}
	return fmt.Errorf("%s %s failed: %s (%s)", c.app, op, message, code)
}

// AppKeepsToolkitData reports whether the application's engine holds data PER
// TOOLKIT, so that deleting a toolkit owes the engine a DeleteToolkit call.
// Inventory does: each toolkit is a graph. DeepWiki does not: its search index
// is per wiki and goes with the project (DeleteProject), and its DeleteToolkit
// answers UNIMPLEMENTED — so a DeepWiki toolkit delete makes no engine call at
// all, rather than one that is known to be refused.
func AppKeepsToolkitData(app string) bool { return app == AppInventory }

// toolkitTypeApps is the allow-list of the toolkit types the two applications
// publish, each to its application: EXACT type names, not prefixes.
//
// WHY A LIST. elitea-main has no in-process registry of toolkit types: a
// provider's types are projected from the admitted manifests in the database
// (internal/api/v2/toolkits.projectProviderToolkits reads centry's provider-hub
// rows for the public project), per request, and a toolkit delete must decide
// inside its own transaction, from the deleted row's `type` alone, whether an
// engine is owed a call. A prefix match ("inventory_*") would also claim any
// future toolkit of another provider whose name happens to start the same way,
// and a lookup in the manifests would make a delete depend on the admission
// table. So the types are pinned here, from the descriptors this repository
// ships (services/elitea-subapp-host/internal/apps/<app>/descriptor.json), and
// TestAppForToolkitTypeCoversTheShippedDescriptors fails when a descriptor
// publishes a type this list does not know.
//
// A projected type is the descriptor's `toolkit_metadata.type_override`, or
// `<provider name>_<toolkit name>` without one. Inventory's two toolkits both
// override: `inventory` (a graph) and `inventory_search` (a search over another
// toolkit's graph; the engine holds nothing for it and answers "nothing to
// delete", which is the correct close of its row). DeepWiki's descriptor is
// named `wikis`; its `Wikis` toolkit has no override and the other two do. The
// historical spellings `deepwiki_Deepwiki`, `deepwiki` and `wikis` are the ones
// internal/api/v2/deepwiki.AdmittedWikisToolkitTypes still admits for stored
// rows.
var toolkitTypeApps = map[string]string{
	"inventory":        AppInventory,
	"inventory_search": AppInventory,

	"wikis_Wikis":       AppDeepWiki,
	"wikis_query":       AppDeepWiki,
	"wiki_query":        AppDeepWiki,
	"deepwiki_Deepwiki": AppDeepWiki,
	"deepwiki":          AppDeepWiki,
	"wikis":             AppDeepWiki,
}

// AppForToolkitType names the application whose engine holds data for a
// toolkit of the given type, or "" for a type no platform service covers. The
// match is exact (toolkitTypeApps), after trimming whitespace.
func AppForToolkitType(toolkitType string) string {
	return toolkitTypeApps[strings.TrimSpace(toolkitType)]
}

// ToolkitTypesOf lists the toolkit types AppForToolkitType maps to app, sorted.
// For the tests that pin the list and for a startup log.
func ToolkitTypesOf(app string) []string {
	types := make([]string, 0, len(toolkitTypeApps))
	for toolkitType, owner := range toolkitTypeApps {
		if owner == app {
			types = append(types, toolkitType)
		}
	}
	sort.Strings(types)
	return types
}
