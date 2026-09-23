package storage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

const (
	defaultMaxInputContentBytes = 256 * 1024
	defaultMaxContentRequests   = 16
	maxRuntimeGeneration        = uint64(1<<63 - 1)
	claimIDHeader               = "X-Elitea-Claim-Id"
	fenceHeader                 = "X-Elitea-Fence"
	// runtimeContextAcceptHeader names the OPTIONAL runtime-context fields the
	// worker is able to read. It exists because both workers refuse a response
	// that carries a key they do not know: the Python worker compares the key
	// set (transport/runtime_context.py) and the native worker parses with
	// `deny_unknown_fields` (src/transport/runtime_context.rs). A new field
	// served unconditionally would therefore fail every token redemption on
	// every worker pod a rolling deploy has not yet replaced, which is an
	// outage of all agent execution and not a compatibility inconvenience.
	//
	// The value is a comma-separated list of field names. An unknown name is
	// ignored, so the header never refuses a request.
	runtimeContextAcceptHeader = "X-Elitea-Runtime-Context-Accept"
	// runtimeContextFieldSecretsHeader is the accept-list name for the
	// project's `X-SECRET` value (#408).
	runtimeContextFieldSecretsHeader = "secrets-header-value"
	SourceContentDigestHeader        = "X-Elitea-Source-Content-Digest"
	SourceContentLengthHeader        = "X-Elitea-Source-Content-Length"
	SourceImmutableVersionHeader     = "X-Elitea-Source-Immutable-Version"
)

var (
	ErrContentUnauthorized = errors.New("input content authorization failed")
	ErrContentNotFound     = errors.New("input content not found")
	ErrContentRejected     = errors.New("input content materialization rejected")
	ErrContentUnavailable  = errors.New("input content materialization unavailable")
)

// ContentClaim is the complete claim-bound authorization input. The workload
// identity comes only from the verified peer certificate, never a forwarded
// user/project header.
type ContentClaim struct {
	PeerCertificate  *x509.Certificate
	ExecutionID      string
	Generation       uint64
	ClaimID          string
	FenceToken       []byte
	ContentID        string
	ImmutableVersion string
}

type ContentAuthorization struct {
	ResourceProjectID string
	ToolkitType       string
	// ActorID is the exact durable execution_jobs.actor_id. Materializers own
	// any capability-specific interpretation, including current user lookup.
	ActorID           string
	InputBundleID     string
	CapabilityID      string
	SemanticRole      string
	ExpectedMediaType string
	ExpectedDigest    [sha256.Size]byte
	ExpectedLength    int64
}

// ContentAuthorizer validates workload identity, claim, generation, fence,
// content identity, and project ownership against the durable control state.
type ContentAuthorizer interface {
	AuthorizeContent(context.Context, ContentClaim) (ContentAuthorization, error)
}

// ContentStore opens immutable content only after authorization. Implementors
// must scope lookup by the authorized resource project.
type ContentStore interface {
	OpenContent(context.Context, string, string, string, string) (io.ReadCloser, error)
}

// ContentMaterializer derives one claim-scoped response from already verified
// immutable source bytes. It must not persist or publish the returned bytes.
// The source digest/version remain the durable execution input identity.
type ContentMaterializer interface {
	MaterializeContent(context.Context, ContentAuthorization, []byte, int64) ([]byte, error)
}

type ContentServer struct {
	authorizer       ContentAuthorizer
	store            ContentStore
	materializer     ContentMaterializer
	runtimeToken     *EliteaClientTokenService
	runtimeVersions  *RuntimeApplicationVersionService
	runtimeObjects   *RuntimeAttachmentObjectService
	toolkitArtifacts ToolkitDiscoveryArtifactStore
	runtimeBuilders  *RuntimeEntityBuilderService
	runtimeArtifacts *RuntimeArtifactObjectService
	maxBytes         int64
	requests         chan struct{}
	logger           *slog.Logger
}

func NewContentServer(authorizer ContentAuthorizer, store ContentStore, maxBytes int64) (*ContentServer, error) {
	return NewContentServerWithLimits(authorizer, store, maxBytes, defaultMaxContentRequests)
}

func NewContentServerWithLimits(authorizer ContentAuthorizer, store ContentStore, maxBytes int64, maxConcurrentRequests int) (*ContentServer, error) {
	return newContentServer(authorizer, store, nil, nil, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewMaterializingContentServerWithLimits extends the same private mTLS,
// claim, fence and audience authorization path with last-moment materialization.
// It intentionally does not expose a second authentication contract.
func NewMaterializingContentServerWithLimits(authorizer ContentAuthorizer, store ContentStore, materializer ContentMaterializer, maxBytes int64, maxConcurrentRequests int) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	return newContentServer(authorizer, store, materializer, nil, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewRuntimeContentServerWithLimits enables the private Elitea client-token
// compatibility context on the same bounded mTLS listener as immutable input
// reads. Configuration materialization is deliberately not composed here: it
// belongs behind the generic Configurations domain, not an index/provider
// specific switch.
func NewRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	runtimeToken *EliteaClientTokenService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	return newContentServer(authorizer, store, nil, runtimeToken, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewMaterializingRuntimeContentServerWithLimits composes generic
// claim-scoped materialization and Elitea client-token compatibility on one
// bounded private mTLS listener.
func NewMaterializingRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	return newContentServer(authorizer, store, materializer, runtimeToken, nil, nil, maxBytes, maxConcurrentRequests)
}

// NewNestedAgentRuntimeContentServerWithLimits adds the nested application
// (agent-as-tool) definition route to the same bounded private mTLS listener.
//
// It is a separate constructor rather than an extra argument on the one above
// because the nested route is only meaningful where agent execution is
// dispatched: the index path composes the identical listener and must not grow
// a route it can never authorize. A nil version service leaves the route
// unregistered, which is what the index composition wants and what every
// pre-existing caller keeps.
func NewNestedAgentRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	runtimeVersions *RuntimeApplicationVersionService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	if runtimeVersions == nil {
		return nil, errors.New("runtime application version context is required")
	}
	return newContentServer(
		authorizer,
		store,
		materializer,
		runtimeToken,
		runtimeVersions,
		nil,
		maxBytes,
		maxConcurrentRequests,
	)
}

// NewAgentAttachmentRuntimeContentServerWithLimits adds the claim-scoped
// attachment OBJECT-READ route on top of the nested-agent listener above.
//
// It is a third constructor rather than a nil-able argument on the second for
// the same reason the second exists: the route must be absent, not merely
// unreachable, wherever it cannot be served. It needs object storage, and this
// service runs in deployments where the Go artifacts capability is off and
// dependencies.ObjectStore is nil (mixed deployments keep Centry's artifacts
// authoritative — see runtimecomposition's retention-sweep note). Making the
// dependency required here, and choosing the constructor at composition time,
// means a nil store yields a listener with no attachment route instead of one
// whose route answers 503 forever.
func NewAgentAttachmentRuntimeContentServerWithLimits(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	runtimeVersions *RuntimeApplicationVersionService,
	runtimeObjects *RuntimeAttachmentObjectService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if materializer == nil {
		return nil, errors.New("content materializer is required")
	}
	if runtimeToken == nil {
		return nil, errors.New("runtime context is required")
	}
	if runtimeVersions == nil {
		return nil, errors.New("runtime application version context is required")
	}
	if runtimeObjects == nil {
		return nil, errors.New("runtime attachment object context is required")
	}
	return newContentServer(
		authorizer,
		store,
		materializer,
		runtimeToken,
		runtimeVersions,
		runtimeObjects,
		maxBytes,
		maxConcurrentRequests,
	)
}

func newContentServer(
	authorizer ContentAuthorizer,
	store ContentStore,
	materializer ContentMaterializer,
	runtimeToken *EliteaClientTokenService,
	runtimeVersions *RuntimeApplicationVersionService,
	runtimeObjects *RuntimeAttachmentObjectService,
	maxBytes int64,
	maxConcurrentRequests int,
) (*ContentServer, error) {
	if authorizer == nil || store == nil {
		return nil, errors.New("content authorizer and store are required")
	}
	if maxBytes == 0 {
		maxBytes = defaultMaxInputContentBytes
	}
	if maxBytes < 1 || maxConcurrentRequests < 1 || maxConcurrentRequests > 1024 {
		return nil, errors.New("content size and concurrency limits must be positive and bounded")
	}
	return &ContentServer{
		authorizer:      authorizer,
		store:           store,
		materializer:    materializer,
		runtimeToken:    runtimeToken,
		runtimeVersions: runtimeVersions,
		runtimeObjects:  runtimeObjects,
		maxBytes:        maxBytes,
		requests:        make(chan struct{}, maxConcurrentRequests),
		logger:          slog.Default(),
	}, nil
}

// WithRuntimeEntityBuilders enables the two chat-authored builder WRITE routes
// (#940 A8) on this listener.
//
// It is a post-construction setter rather than a sixth constructor, and that
// is a deliberate break with the pattern the five above established. Each of
// those exists because its route must be ABSENT, not merely unreachable,
// wherever its dependency is missing — and the same is true here — but the
// combinatorics had already reached three call-site branches over two optional
// services; a fourth optional service would have made it six, each one a
// separately-maintained argument list that differs from its neighbours by one
// nil. This setter keeps the same property (a nil service leaves both routes
// unregistered) with one line at each composition site.
//
// It must be called before Routes(). Both composition sites do
// (internal/runtimecomposition/composition.go), and a later call is harmless
// but silently useless, which is why this returns the server: the intended
// shape is one chained expression, not a statement somebody can drift away
// from its constructor.
func (s *ContentServer) WithRuntimeEntityBuilders(builders *RuntimeEntityBuilderService) *ContentServer {
	if s == nil {
		return nil
	}
	s.runtimeBuilders = builders
	return s
}

// WithRuntimeArtifacts enables the four claim-scoped `artifact` toolkit routes
// (#906) on this listener: list, read, write and delete, inside the claimed
// project and under the claimed actor's own per-bucket access list.
//
// Same shape and same reason as WithRuntimeEntityBuilders above: a nil service
// leaves all four routes UNREGISTERED rather than serving a route that answers
// 503 forever. That is not a detail here — this service runs in deployments
// with no Go object store at all (mixed deployments keep Centry's artifacts
// authoritative), and there the honest answer is that the native runtime skips
// the toolkit, not that it has one that always fails.
//
// It must be called before Routes(), which both composition sites do.
func (s *ContentServer) WithRuntimeArtifacts(artifacts *RuntimeArtifactObjectService) *ContentServer {
	if s == nil {
		return nil
	}
	s.runtimeArtifacts = artifacts
	return s
}

// Routes exposes only the internal, claim-bound input data plane.
func (s *ContentServer) Routes() http.Handler {
	r := chi.NewRouter()
	if s.toolkitArtifacts != nil {
		r.Put("/executions/{executionID}/generations/{generation}/inputs/{contentID}/versions/{version}/toolkit-discovery-result", s.PutToolkitDiscoveryArtifact)
		r.Get("/executions/{executionID}/generations/{generation}/inputs/{contentID}/versions/{version}/toolkit-discovery-result", s.GetToolkitDiscoveryArtifact)
	}
	r.Get("/executions/{executionID}/generations/{generation}/inputs/{contentID}/versions/{version}", s.Get)
	if s.runtimeToken != nil {
		r.Post("/executions/{executionID}/generations/{generation}/runtime-context/elitea-client-token", s.PostEliteaClientToken)
	}
	if s.runtimeVersions != nil {
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/applications/{applicationID}/versions/{versionID}",
			s.PostApplicationVersion,
		)
	}
	if s.runtimeObjects != nil {
		// {name} is ONE path parameter holding a key that contains slashes
		// (`{conversationUUID}/{filename}`), percent-encoded by the client.
		// chi routes on r.URL.RawPath when it is non-empty, so `%2F` stays one
		// segment here and PostAttachmentObject unescapes it — the same
		// mechanism the immutable input route already relies on for
		// {contentID} and {version}, which is why claimPathPart exists.
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/attachments/{bucket}/{name}",
			s.PostAttachmentObject,
		)
	}
	if s.runtimeBuilders != nil {
		// The only two routes on this listener that take a request BODY.
		// Everything else here is a read whose whole selection fits in the
		// path, which is why parseExecutionClaim's callers all refuse a body
		// outright; these two carry the document being written, so they read
		// one under an explicit cap instead.
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/skills",
			s.PostSkillWrite,
		)
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/project-context",
			s.PostProjectContextWrite,
		)
	}
	if s.runtimeArtifacts != nil {
		// The `artifact` toolkit family's four operations (#906). They carry
		// a BODY like the two builder writes above, and for the same reason:
		// a bucket name, a key and — for the write — the document itself do
		// not belong in a path, and one of them is the payload rather than a
		// selection. The operation is the last path segment so the four share
		// one prefix and one claim contract.
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/artifacts/list",
			s.PostArtifactList,
		)
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/artifacts/read",
			s.PostArtifactRead,
		)
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/artifacts/write",
			s.PostArtifactWrite,
		)
		r.Post(
			"/executions/{executionID}/generations/{generation}/runtime-context/artifacts/delete",
			s.PostArtifactDelete,
		)
	}
	return r
}

func (s *ContentServer) Get(w http.ResponseWriter, r *http.Request) {
	if !s.acquire(w) {
		return
	}
	defer s.release()
	claim, err := parseContentClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	authorization, err := s.authorizer.AuthorizeContent(r.Context(), claim)
	if err != nil || authorization.ResourceProjectID == "" || authorization.InputBundleID == "" || authorization.CapabilityID == "" || authorization.SemanticRole == "" || authorization.ExpectedMediaType == "" || authorization.ExpectedLength <= 0 {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(authorization.ExpectedMediaType)
	if err != nil || mediaType != authorization.ExpectedMediaType || len(parameters) != 0 {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if authorization.ExpectedLength > s.maxBytes {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}

	content, err := s.store.OpenContent(
		r.Context(),
		authorization.ResourceProjectID,
		authorization.InputBundleID,
		claim.ContentID,
		claim.ImmutableVersion,
	)
	if errors.Is(err, ErrContentNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer func() {
		if closeErr := content.Close(); closeErr != nil {
			s.logger.WarnContext(r.Context(), "input content close failed")
		}
	}()

	// This validation slice is deliberately small. Buffering here lets the
	// server verify length/digest before sending any bytes to the worker; larger
	// artifact paths use a separately resumable streaming contract.
	data, err := io.ReadAll(io.LimitReader(content, s.maxBytes+1))
	if err != nil || int64(len(data)) != authorization.ExpectedLength || int64(len(data)) > s.maxBytes {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256(data)
	if subtle.ConstantTimeCompare(digest[:], authorization.ExpectedDigest[:]) != 1 {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	responseData := data
	if s.materializer != nil {
		responseData, err = s.materializer.MaterializeContent(r.Context(), authorization, data, s.maxBytes)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrContentRejected) {
				status = http.StatusUnprocessableEntity
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		if len(responseData) == 0 || int64(len(responseData)) > s.maxBytes {
			http.Error(w, http.StatusText(http.StatusUnprocessableEntity), http.StatusUnprocessableEntity)
			return
		}
	}
	responseDigest := sha256.Sum256(responseData)
	defer clearContentBytes(responseData)

	w.Header().Set("Content-Type", authorization.ExpectedMediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(responseData)))
	w.Header().Set("Content-Digest", formatSHA256Digest(responseDigest))
	w.Header().Set(SourceContentDigestHeader, formatSHA256Digest(digest))
	w.Header().Set(SourceContentLengthHeader, strconv.FormatInt(authorization.ExpectedLength, 10))
	w.Header().Set(SourceImmutableVersionHeader, claim.ImmutableVersion)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseData)
}

func (s *ContentServer) PostEliteaClientToken(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.runtimeToken == nil || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	value, err := s.runtimeToken.Resolve(r.Context(), claim)
	if err == nil && !runtimeContextAccepts(r, runtimeContextFieldSecretsHeader) {
		// The worker did not say it can read this field, so it does not get it.
		// See runtimeContextAcceptHeader above for why silence means "no".
		value.SecretsHeaderValue = ""
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ErrContentUnauthorized) {
			status = http.StatusForbidden
		} else if errors.Is(err, ErrContentUnavailable) {
			s.logger.WarnContext(
				r.Context(),
				"runtime context unavailable",
				"stage",
				runtimeContextUnavailableStage(err),
			)
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeContextResponseBytes {
		clearContentBytes(encoded)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer clearContentBytes(encoded)
	digest := sha256.Sum256(encoded)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.Header().Set("Content-Digest", formatSHA256Digest(digest))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// PostApplicationVersion serves one frozen nested (agent-as-tool) child
// definition under the live claim that already authorized the parent turn.
//
// It is the exact server twin of PostEliteaClientToken above — same claim
// parsing, same concurrency gate, same private-no-cache headers, same error
// taxonomy — and that symmetry is the point: the worker reaches both routes
// over one mTLS channel with one authority, and a route that authorized
// differently would be a second, weaker contract on the same connection.
func (s *ContentServer) PostApplicationVersion(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.runtimeVersions == nil || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	applicationID, applicationIDOK := claimPathIdentity(r, "applicationID")
	versionID, versionIDOK := claimPathIdentity(r, "versionID")
	if !applicationIDOK || !versionIDOK {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	value, err := s.runtimeVersions.Resolve(r.Context(), claim, applicationID, versionID)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrContentUnauthorized):
			status = http.StatusForbidden
		case errors.Is(err, ErrContentNotFound):
			// The claim was good and the agent/version pair was not. Kept
			// distinct from 403 so an operator can tell a stale nested
			// reference from a rejected claim; the worker collapses both into
			// its own failure taxonomy either way.
			status = http.StatusNotFound
		case errors.Is(err, ErrContentUnavailable):
			s.logger.WarnContext(
				r.Context(),
				"nested application version unavailable",
				"stage",
				runtimeContextUnavailableStage(err),
			)
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeApplicationVersionResponseBytes {
		// Refused, never trimmed: the body is one indivisible definition, and a
		// truncated one would either fail the client's JSON decode or, worse,
		// parse into an agent missing tools its author attached.
		clearContentBytes(encoded)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer clearContentBytes(encoded)
	digest := sha256.Sum256(encoded)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.Header().Set("Content-Digest", formatSHA256Digest(digest))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		s.logger.WarnContext(r.Context(), "nested application version write failed")
	}
}

// PostAttachmentObject serves one stored chat attachment's text under the live
// claim that already authorized the turn the file rides on.
//
// It is the server twin of PostApplicationVersion above — same claim parsing,
// same concurrency gate, same private-no-cache headers, same error taxonomy
// plus one status — and that symmetry is the point: the worker reaches every
// runtime-context route over one mTLS channel with one authority, and a route
// that authorized differently would be a second, weaker contract on the same
// connection.
//
// THE ONE ADDED STATUS IS 422. A stored object that cannot be served as text —
// oversized, empty, or not UTF-8 — is neither a rejected claim nor a missing
// file, and the difference matters operationally: 404 says the reference is
// stale and someone should look at the row, while 422 says the file is exactly
// what it claims to be and this route will never be able to read it. The worker
// treats both as "unreadable" and announces the file by name either way, so
// neither ever fails a turn.
func (s *ContentServer) PostAttachmentObject(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.runtimeObjects == nil || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	bucket, bucketErr := claimPathPart(r, "bucket")
	name, nameErr := claimPathPart(r, "name")
	if bucketErr != nil || nameErr != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	value, err := s.runtimeObjects.Resolve(r.Context(), claim, bucket, name)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrContentUnauthorized):
			// Also the CROSS-CONVERSATION refusal. It is deliberately the same
			// 403 a stale or foreign claim gets: a caller must not be able to
			// learn, from the status alone, that an object exists in a
			// conversation it does not hold a claim for.
			status = http.StatusForbidden
		case errors.Is(err, ErrContentNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrContentRejected):
			status = http.StatusUnprocessableEntity
		case errors.Is(err, ErrContentUnavailable):
			s.logger.WarnContext(
				r.Context(),
				"attachment object unavailable",
				"stage",
				runtimeContextUnavailableStage(err),
			)
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeAttachmentObjectResponseBytes {
		// Refused, never trimmed. A truncated document is worse than an
		// unread one: the model is shown a prefix and told it is the file.
		clearContentBytes(encoded)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer clearContentBytes(encoded)
	digest := sha256.Sum256(encoded)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.Header().Set("Content-Digest", formatSHA256Digest(digest))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		s.logger.WarnContext(r.Context(), "attachment object write failed")
	}
}

// PostSkillWrite creates or updates one Skill from the conversation the claim
// authorizes (#940 A8, `skills_builder`).
//
// It is the first WRITE on this listener, and it keeps every property the
// reads have: same claim parsing, same concurrency gate, same private
// no-cache headers, same error taxonomy. What it adds is a bounded request
// body — refused, never truncated, for the same reason the attachment read
// refuses an oversized file rather than sending a prefix.
func (s *ContentServer) PostSkillWrite(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.runtimeBuilders == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	var request RuntimeSkillWriteRequest
	if !decodeBuilderRequest(w, r, &request) {
		return
	}
	value, err := s.runtimeBuilders.WriteSkill(r.Context(), claim, request)
	if err != nil {
		s.writeBuilderError(w, r, err, "skill write unavailable")
		return
	}
	s.writeBuilderResponse(w, r, value, "skill write response failed")
}

// PostProjectContextWrite writes the claimed project's Project Context
// (#940 A8, `project_context_builder`). Twin of PostSkillWrite above.
func (s *ContentServer) PostProjectContextWrite(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.runtimeBuilders == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	var request RuntimeProjectContextWriteRequest
	if !decodeBuilderRequest(w, r, &request) {
		return
	}
	value, err := s.runtimeBuilders.WriteProjectContext(r.Context(), claim, request)
	if err != nil {
		s.writeBuilderError(w, r, err, "project context write unavailable")
		return
	}
	s.writeBuilderResponse(w, r, value, "project context write response failed")
}

// PostArtifactList, PostArtifactRead, PostArtifactWrite and PostArtifactDelete
// are the `artifact` toolkit family's four operations (#906).
//
// They are twins of PostSkillWrite above in every respect that authorizes
// anything — same claim parsing, same concurrency gate, same private no-cache
// headers, same bounded body refused rather than truncated, same error
// taxonomy — and differ only in the document each carries. The symmetry is the
// point: one mTLS channel, one authority, and no route on it that authorizes
// differently from its neighbours.
func (s *ContentServer) PostArtifactList(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	var request RuntimeArtifactListRequest
	claim, ok := s.artifactRequest(w, r, &request)
	if !ok {
		return
	}
	value, err := s.runtimeArtifacts.List(r.Context(), claim, request)
	if err != nil {
		s.writeBuilderError(w, r, err, "artifact listing unavailable")
		return
	}
	s.writeRuntimeResponse(w, r, value, maxRuntimeArtifactResponseBytes, "artifact listing response failed")
}

func (s *ContentServer) PostArtifactRead(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	var request RuntimeArtifactReadRequest
	claim, ok := s.artifactRequest(w, r, &request)
	if !ok {
		return
	}
	value, err := s.runtimeArtifacts.Read(r.Context(), claim, request)
	if err != nil {
		s.writeBuilderError(w, r, err, "artifact read unavailable")
		return
	}
	s.writeRuntimeResponse(w, r, value, maxRuntimeArtifactResponseBytes, "artifact read response failed")
}

func (s *ContentServer) PostArtifactWrite(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	var request RuntimeArtifactWriteRequest
	claim, ok := s.artifactRequest(w, r, &request)
	if !ok {
		return
	}
	value, err := s.runtimeArtifacts.Write(r.Context(), claim, request)
	if err != nil {
		s.writeBuilderError(w, r, err, "artifact write unavailable")
		return
	}
	s.writeRuntimeResponse(w, r, value, maxRuntimeArtifactResponseBytes, "artifact write response failed")
}

func (s *ContentServer) PostArtifactDelete(w http.ResponseWriter, r *http.Request) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	var request RuntimeArtifactDeleteRequest
	claim, ok := s.artifactRequest(w, r, &request)
	if !ok {
		return
	}
	value, err := s.runtimeArtifacts.Delete(r.Context(), claim, request)
	if err != nil {
		s.writeBuilderError(w, r, err, "artifact delete unavailable")
		return
	}
	s.writeRuntimeResponse(w, r, value, maxRuntimeArtifactResponseBytes, "artifact delete response failed")
}

// artifactRequest is the claim-and-body half all four artifact routes share.
//
// Four copies of it would be four places for one to drift out of step with the
// others, and the thing that would drift is the authorization. It writes the
// refusal itself and returns ok=false; the caller returns immediately.
func (s *ContentServer) artifactRequest(
	w http.ResponseWriter,
	r *http.Request,
	target any,
) (ContentClaim, bool) {
	if s.runtimeArtifacts == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return ContentClaim{}, false
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return ContentClaim{}, false
	}
	if !decodeRuntimeRequest(w, r, target, maxRuntimeArtifactRequestBytes) {
		return ContentClaim{}, false
	}
	return claim, true
}

// decodeBuilderRequest reads one bounded JSON body, refusing unknown keys.
//
// DisallowUnknownFields is the mirror of the worker”'s own
// `deny_unknown_fields` on every response: both directions refuse a document
// carrying a field the other side does not know, so a version skew fails
// loudly at the edge instead of half-applying a write.
func decodeBuilderRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeRuntimeRequest(w, r, target, maxRuntimeBuilderRequestBytes)
}

// decodeRuntimeRequest is decodeBuilderRequest with the cap named by the
// caller: the two builder writes carry prompt-sized documents, the artifact
// write carries a file, and one number could not honestly bound both.
func decodeRuntimeRequest(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) bool {
	if r.ContentLength > maxBytes {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return false
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return false
	}
	return true
}

// writeBuilderError maps one builder failure onto the listener”'s shared
// taxonomy. 422 (ErrContentRejected) is the write-side addition and it is
// deliberately NOT an error the worker fails a turn on: it means the document
// the model composed cannot be stored as written, which the model can read and
// retry from.
func (s *ContentServer) writeBuilderError(w http.ResponseWriter, r *http.Request, err error, message string) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, ErrContentUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, ErrContentNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrContentRejected):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, ErrContentUnavailable):
		s.logger.WarnContext(r.Context(), message, "stage", runtimeContextUnavailableStage(err))
	}
	http.Error(w, http.StatusText(status), status)
}

func (s *ContentServer) writeBuilderResponse(w http.ResponseWriter, r *http.Request, value any, message string) {
	s.writeRuntimeResponse(w, r, value, maxRuntimeBuilderResponseBytes, message)
}

// writeRuntimeResponse is writeBuilderResponse with the envelope ceiling named
// by the caller. An over-cap body is REFUSED rather than trimmed, for the
// reason PostApplicationVersion states: the document is indivisible, and a
// truncated one either fails the client's decode or — worse — parses into
// something that looks complete.
func (s *ContentServer) writeRuntimeResponse(
	w http.ResponseWriter,
	r *http.Request,
	value any,
	maxBytes int,
	message string,
) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maxBytes {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	digest := sha256.Sum256(encoded)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.Header().Set("Content-Digest", formatSHA256Digest(digest))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		s.logger.WarnContext(r.Context(), message)
	}
}

func (s *ContentServer) acquire(w http.ResponseWriter) bool {
	select {
	case s.requests <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return false
	}
}

func (s *ContentServer) release() {
	<-s.requests
}

func setPrivateNoCacheHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "0")
}

func formatSHA256Digest(digest [sha256.Size]byte) string {
	return "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
}

func clearContentBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func parseContentClaim(r *http.Request) (ContentClaim, error) {
	claim, err := parseExecutionClaim(r)
	if err != nil {
		return ContentClaim{}, err
	}
	claim.ContentID, err = claimPathPart(r, "contentID")
	if err != nil {
		return ContentClaim{}, err
	}
	claim.ImmutableVersion, err = claimPathPart(r, "version")
	if err != nil {
		return ContentClaim{}, fmt.Errorf("%w: incomplete claim", ErrContentUnauthorized)
	}
	return claim, nil
}

func parseExecutionClaim(r *http.Request) (ContentClaim, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ContentClaim{}, ErrContentUnauthorized
	}
	generationText := chi.URLParam(r, "generation")
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil || generation == 0 || generation > maxRuntimeGeneration || strconv.FormatUint(generation, 10) != generationText {
		return ContentClaim{}, ErrContentUnauthorized
	}
	claimID, claimIDOK := singleHeader(r.Header, claimIDHeader)
	fenceText, fenceOK := singleHeader(r.Header, fenceHeader)
	fence, err := base64.RawURLEncoding.DecodeString(fenceText)
	if !claimIDOK || !fenceOK || err != nil || len(fence) != sha256.Size || base64.RawURLEncoding.EncodeToString(fence) != fenceText {
		return ContentClaim{}, ErrContentUnauthorized
	}
	if !boundedClaimPart(claimID) {
		return ContentClaim{}, fmt.Errorf("%w: incomplete claim", ErrContentUnauthorized)
	}
	executionID, err := claimPathPart(r, "executionID")
	if err != nil {
		return ContentClaim{}, err
	}
	return ContentClaim{
		PeerCertificate: r.TLS.VerifiedChains[0][0],
		ExecutionID:     executionID,
		Generation:      generation,
		ClaimID:         claimID,
		FenceToken:      fence,
	}, nil
}

func claimPathPart(r *http.Request, name string) (string, error) {
	value, err := url.PathUnescape(chi.URLParam(r, name))
	if err != nil || !boundedClaimPart(value) {
		return "", fmt.Errorf("%w: incomplete claim", ErrContentUnauthorized)
	}
	return value, nil
}

// claimPathIdentity reads one canonical positive integer path segment.
//
// Canonical, not merely parseable: the worker formats these with plain
// `{application_id}` interpolation (runtime_context.rs:454-456), so "007" or
// "+7" never come from it. Admitting them would let two spellings of the same
// identity address one agent, and the response echoes the identity back for the
// client to compare (:554-564) — a comparison that only means something while
// exactly one spelling reaches the query.
func claimPathIdentity(r *http.Request, name string) (uint64, bool) {
	text := chi.URLParam(r, name)
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != text {
		return 0, false
	}
	return value, true
}

// runtimeContextAccepts reports whether the request asked for one optional
// runtime-context field by name.
//
// It reads every value of the header, not one, because a proxy is free to split
// a comma list across repeated header lines. Names are compared case-blind and
// with the surrounding space removed, and an empty or absent header accepts
// nothing.
func runtimeContextAccepts(r *http.Request, field string) bool {
	for _, line := range r.Header.Values(runtimeContextAcceptHeader) {
		for _, name := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(name), field) {
				return true
			}
		}
	}
	return false
}

func singleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func boundedClaimPart(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := range len(value) {
		switch value[index] {
		case '\r', '\n', 0:
			return false
		}
	}
	return true
}
