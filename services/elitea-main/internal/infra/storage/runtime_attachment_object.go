package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
)

const (
	// RuntimeAttachmentObjectSchemaVersion is the exact discriminator the
	// worker compares before it will read the body at all
	// (ATTACHMENT_OBJECT_SCHEMA_V2,
	// services/elitea-worker-rust/src/transport/runtime_context.rs).
	//
	// v2 replaced v1 when this route started to serve EXTRACTED text: v1
	// carried the raw bytes of a UTF-8 file up to 128 KiB and nothing else;
	// v2 carries the extracted text plus the page/section/sheet map, the
	// source's unit count, the low-text pages and whether the text is
	// complete. A v1 worker rejects a v2 document by its discriminator and
	// falls back to naming the file, which is the pre-#606 behaviour.
	RuntimeAttachmentObjectSchemaVersion = "elitea.runtime.attachment-object.v2"

	// RuntimeAttachmentUnreadableSchemaVersion is the 422 body: the reason a
	// stored attachment has no text this route can serve.
	RuntimeAttachmentUnreadableSchemaVersion = "elitea.runtime.attachment-unreadable.v1"

	// maxRuntimeAttachmentInputBytes bounds the SOURCE OBJECT that is read
	// for extraction. It is the extractor's own input limit
	// (extract.DefaultLimits().MaxInputBytes, 25 MiB); a larger upload is
	// refused with reason too_large, from the metadata row alone.
	maxRuntimeAttachmentInputBytes = 25 << 20

	// maxRuntimeAttachmentServedTextBytes bounds the TEXT one response
	// carries: about 500k tokens, which is more than any model's attachment
	// budget, so the worker can inline what fits and page through the rest
	// with its read/search tools. A longer extraction is served up to this
	// limit at a unit boundary, and the document says so (complete=false and
	// the units it covers). It is never cut without saying so.
	maxRuntimeAttachmentServedTextBytes = 2 << 20

	// maxRuntimeAttachmentObjectResponseBytes is the envelope's own ceiling,
	// restated here so this side REFUSES rather than sends a body the client
	// will reject after buffering it (MAX_ATTACHMENT_OBJECT_BYTES on the worker
	// side). The text is encoded without HTML escaping and has no control
	// characters other than newline and tab (extract.normalize), so it
	// encodes to at most twice its size; the unit map adds well under 1 MiB
	// (at most extract's unit limit of short labels).
	maxRuntimeAttachmentObjectResponseBytes = 6 << 20

	// MaxConcurrentAttachmentExtractions bounds the extractions that run at
	// the same time, and with them the source bytes held (a slot is taken
	// before the object is read). Only extract.PDFEngineConcurrency of them
	// parse a PDF at once. extract.PeakMemoryBytes of this number is what
	// the Helm chart's memory limit for elitea-main is tested against.
	MaxConcurrentAttachmentExtractions = 2

	// attachmentExtractionWait is below the worker's 15 s runtime-context
	// deadline (content_timeout_millis), so the "processing" answer reaches
	// the worker before it gives up on the request. It bounds the WHOLE
	// wait, the wait for an extraction slot included.
	attachmentExtractionWait = 10 * time.Second

	runtimeContextStageAttachmentRead         = "attachment_object_read"
	runtimeContextStageAttachmentExtract      = "attachment_object_extract"
	runtimeContextStageAttachmentConversation = "attachment_conversation"

	// canonicalUUIDLength is 8-4-4-4-12 with its four separators. The claim's
	// conversation identity is compared against this shape before it is used
	// as an authorization prefix — see conversationScopedAttachmentKey.
	canonicalUUIDLength = 36
)

// AttachmentObjectRecord is one stored chat attachment exactly as object
// storage holds it, already bounded by the caller's cap.
//
// MediaType is the value the UPLOAD recorded (elitea_storage.objects.media_type),
// carried for the worker's diagnostics only. It is deliberately not the gate on
// what may be returned: it is whatever the browser put in the multipart part,
// so a .md arrives as application/octet-stream on one client and text/markdown
// on another. The bytes decide instead — see the extract package.
type AttachmentObjectRecord struct {
	Bucket     string
	Name       string
	MediaType  string
	ByteLength int64
	Content    []byte
}

// AttachmentObjectSource opens one stored chat attachment inside the project
// the CLAIM selected. Implementations must not accept a project from the
// request: the caller passes the authorized one, and must additionally refuse
// any object that the chat upload path did not write.
type AttachmentObjectSource interface {
	ReadAttachmentObject(
		ctx context.Context,
		projectID int64,
		bucket string,
		name string,
		maxBytes int64,
	) (AttachmentObjectRecord, error)
}

// DocumentExtractor turns an attachment's bytes into text and a unit map.
// *extract.Extractor is the production implementation.
type DocumentExtractor interface {
	Extract(ctx context.Context, data []byte) (extract.Document, error)
}

// AttachmentObjectVersion identifies the exact stored object an extraction
// was made from. A re-upload under the same key changes updated_at (the
// upsert in queries/artifact_storage.sql sets it), so an extraction of the
// old bytes never answers for the new ones.
type AttachmentObjectVersion struct {
	ObjectID   int64
	ByteLength int64
	UpdatedAt  int64 // Unix microseconds, PostgreSQL's own precision.
	// MediaType is the upload's recorded media type, for diagnostics only.
	MediaType string
}

// AttachmentExtraction is the stored outcome of one extraction: a document,
// or the reason there is none.
type AttachmentExtraction struct {
	Refused  bool
	Reason   extract.Reason
	Document extract.Document
	// ExtractedAt is when a filed extraction was made. It is set on a load
	// only, and it decides when a filed timeout is tried again.
	ExtractedAt time.Time
}

// AttachmentExtractionCache is the sidecar store. Load answers for the
// object the (project, bucket, name) triple resolves NOW, under the same
// project/system-bucket/metadata-row rules as AttachmentObjectSource, and
// returns its version even on a miss so that Save can file under it.
//
// LoadAttachmentExtractionByDigest answers for the same object when only
// its updated_at changed: the chat client uploads a file again with every
// message it rides on, and a byte-identical upload must not be extracted
// again. A hit is filed under the given version before it is returned.
//
// Save records the SHA-256 of the bytes it was made from (digest may be
// nil). The digest, not the version, is what makes a filed extraction
// trustworthy, so Save never replaces a row filed for a newer version.
type AttachmentExtractionCache interface {
	LoadAttachmentExtraction(
		ctx context.Context,
		projectID int64,
		bucket string,
		name string,
		extractorVersion string,
	) (AttachmentExtraction, AttachmentObjectVersion, bool, error)
	LoadAttachmentExtractionByDigest(
		ctx context.Context,
		version AttachmentObjectVersion,
		extractorVersion string,
		digest []byte,
	) (AttachmentExtraction, bool, error)
	SaveAttachmentExtraction(
		ctx context.Context,
		version AttachmentObjectVersion,
		digest []byte,
		extraction AttachmentExtraction,
	) error
}

// AttachmentUnreadableError is ErrContentRejected with the reason. The
// content server sends the reason in the 422 body so the worker can tell the
// model WHY a file has no text ("encrypted", "unsupported_format", ...).
type AttachmentUnreadableError struct {
	Reason extract.Reason
}

func (e *AttachmentUnreadableError) Error() string {
	return "attachment unreadable: " + string(e.Reason)
}

func (e *AttachmentUnreadableError) Is(target error) bool {
	return target == ErrContentRejected
}

func unreadable(reason extract.Reason) error {
	return &AttachmentUnreadableError{Reason: reason}
}

// RuntimeAttachmentUnreadable is the 422 body.
type RuntimeAttachmentUnreadable struct {
	SchemaVersion string `json:"schema_version"`
	Reason        string `json:"reason"`
}

// RuntimeAttachmentUnit is one page, slide, sheet or section of the served
// text. Start and End are byte offsets into Content.
type RuntimeAttachmentUnit struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	HasText bool   `json:"has_text"`
}

// RuntimeAttachmentObjectContext is the wire document. Its fields are the
// complete set the worker accepts: `AttachmentObjectResponse` is
// `deny_unknown_fields`, so one extra key here fails every attachment read with
// a malformed-response error that names nothing.
//
// Content is EXTRACTED TEXT, never raw bytes. Units cover Content in order;
// UnitCount is how many units the SOURCE has. When Complete is false the
// worker must say which units the model has not seen (PartialReason says
// why: page_limit, text_limit, cell_limit, or served_limit for a text longer
// than one response carries).
type RuntimeAttachmentObjectContext struct {
	SchemaVersion    string                  `json:"schema_version"`
	ProjectID        int64                   `json:"project_id"`
	Bucket           string                  `json:"bucket"`
	Name             string                  `json:"name"`
	MediaType        string                  `json:"media_type"`
	ByteLength       int64                   `json:"byte_length"`
	Format           string                  `json:"format"`
	ExtractorVersion string                  `json:"extractor_version"`
	Content          string                  `json:"content"`
	Units            []RuntimeAttachmentUnit `json:"units"`
	UnitCount        int                     `json:"unit_count"`
	LowTextUnits     []int                   `json:"low_text_units"`
	TextBytes        int64                   `json:"text_bytes"`
	TokenEstimate    int64                   `json:"token_estimate"`
	Complete         bool                    `json:"complete"`
	PartialReason    string                  `json:"partial_reason"`
}

// RuntimeAttachmentObjectService serves one stored chat attachment's
// EXTRACTED TEXT to the native runtime, under the same durable claim that
// already authorized the turn the file was attached to.
//
// It exists because the native runtime has no other way to read those bytes:
// it holds no vault, materializes no `artifact` toolkit family, and its egress
// allowlist reaches the model gateway alone.
//
// EXTRACTION HAPPENS HERE, ONCE. The first read of an attachment extracts it
// (internal/infra/extract) and files the result in the sidecar store
// (elitea_storage.attachment_extractions, keyed by the object's identity and
// the extractor version, with the SHA-256 of the bytes); every later turn
// reads the sidecar, and a byte-identical re-upload finds it by that digest.
// A refusal is filed too, so a broken file is not parsed on every turn; a
// timeout is filed and tried again after attachmentTimeoutRetryAfter.
//
// THE AUTHORIZATION IS THE WHOLE POINT, and it has three independent parts:
//
//   - WHICH CLAIM. The authorizer is the agent-scoped one, so an index.ingest.v1
//     claim — which carries a perfectly real resource_project_id — is refused
//     before any lookup happens. An index workload has no conversation and no
//     business reading chat attachments.
//   - WHICH PROJECT. Taken from the claimed execution row and passed to the
//     source, never read from the request. `storage.ObjectRef` is
//     project-scoped by construction, so the bytes of another tenant are not
//     addressable from here at all.
//   - WHICH CONVERSATION. The claim's own conversation (the agent execution
//     row's client_stream_id) must PREFIX the object key. That is the same
//     sentence admission enforces when it accepts the attachment in the first
//     place (internal/application/agentexecution/attachments.go,
//     currentTurnAttachments): the upload endpoint keys every chat object
//     `{conversationUUID}/{filename}`, so requiring the prefix is exactly
//     "this file was uploaded to this conversation".
//
// The request selects only the (bucket, name) pair, and it selects INSIDE that
// project and that conversation.
type RuntimeAttachmentObjectService struct {
	authorizer AgentRuntimeContextAuthorizer
	objects    AttachmentObjectSource
	extractor  DocumentExtractor
	cache      AttachmentExtractionCache
	maxBytes   int64
	maxServed  int
	// extractions bounds the extractions that run at the same time, each
	// holding up to maxBytes of source in memory.
	extractions chan struct{}
	// extractionWait is how long one request waits for an extraction before
	// it answers "processing" and leaves the extraction to file its result.
	extractionWait time.Duration
	// flights joins the requests for one object version to one extraction.
	flights singleflight.Group
	// queued counts the extractions that wait for a slot.
	queued atomic.Int64
	logger *slog.Logger
	now    func() time.Time
}

// NewRuntimeAttachmentObjectService wires the route. cache may be nil (every
// read then extracts again); the other three are required.
func NewRuntimeAttachmentObjectService(
	authorizer AgentRuntimeContextAuthorizer,
	objects AttachmentObjectSource,
	extractor DocumentExtractor,
	cache AttachmentExtractionCache,
) (*RuntimeAttachmentObjectService, error) {
	if authorizer == nil || objects == nil || extractor == nil {
		return nil, errors.New("runtime attachment object dependencies are required")
	}
	return &RuntimeAttachmentObjectService{
		authorizer: authorizer,
		objects:    objects,
		extractor:  extractor,
		cache:      cache,
		maxBytes:   maxRuntimeAttachmentInputBytes,
		maxServed:  maxRuntimeAttachmentServedTextBytes,
		extractions: make(
			chan struct{}, MaxConcurrentAttachmentExtractions,
		),
		extractionWait: attachmentExtractionWait,
		logger:         slog.Default(),
		now:            time.Now,
	}, nil
}

// Resolve reads one stored attachment's text for the claimed execution.
//
// The order is the security boundary: authorize first, and take the project and
// the conversation ONLY from what the claim resolved.
//
// The error taxonomy is chosen so that an operator can tell the failures
// apart, because they mean very different things:
//
//	ErrContentUnauthorized  the claim was rejected, OR it was good and named a
//	                        different conversation than the object does. Both
//	                        are 403: a caller must not be able to probe which
//	                        conversation an object belongs to by reading status
//	                        codes.
//	ErrContentNotFound      the claim was good, the conversation matched, and
//	                        there is no such object.
//	*AttachmentUnreadableError (Is ErrContentRejected)
//	                        the object exists and has no text this route can
//	                        serve — encrypted, an image, an unsupported or
//	                        malformed format, over the input limit, or no text
//	                        layer. The reason travels to the worker, which
//	                        tells the model.
func (service *RuntimeAttachmentObjectService) Resolve(
	ctx context.Context,
	claim ContentClaim,
	bucket string,
	name string,
) (RuntimeAttachmentObjectContext, error) {
	if service == nil || service.authorizer == nil || service.objects == nil ||
		service.extractor == nil || service.maxBytes <= 0 || service.maxServed <= 0 ||
		service.extractions == nil {
		return RuntimeAttachmentObjectContext{}, runtimeContextUnavailable(
			runtimeContextStageAttachmentRead,
		)
	}
	if err := ctx.Err(); err != nil {
		return RuntimeAttachmentObjectContext{}, err
	}
	if !addressableAttachmentBucket(bucket) || !addressableAttachmentKey(name) {
		return RuntimeAttachmentObjectContext{}, ErrContentNotFound
	}
	authorization, err := service.authorizer.AuthorizeAgentRuntimeContext(ctx, claim)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeAttachmentObjectContext{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) {
			return RuntimeAttachmentObjectContext{}, ErrContentUnauthorized
		}
		return RuntimeAttachmentObjectContext{}, runtimeContextUnavailable(
			runtimeContextStageClaimAuthorize,
		)
	}
	if authorization.ResourceProjectID <= 0 ||
		authorization.ResourceProjectID > math.MaxInt32 {
		return RuntimeAttachmentObjectContext{}, runtimeContextUnavailable(
			runtimeContextStageProjectIdentity,
		)
	}
	// A claim with no usable conversation is an UNAVAILABLE, not a refusal:
	// every agent execution row carries a NOT NULL client_stream_id
	// (migrations/shared/0055_agent_execution_admission.sql), so an empty or
	// malformed one means this service and the admission path disagree about
	// the row's shape, which an operator has to see rather than read as a
	// caller's mistake.
	if !canonicalConversationIdentity(authorization.ConversationID) {
		return RuntimeAttachmentObjectContext{}, runtimeContextUnavailable(
			runtimeContextStageAttachmentConversation,
		)
	}
	if !conversationScopedAttachmentKey(authorization.ConversationID, name) {
		return RuntimeAttachmentObjectContext{}, ErrContentUnauthorized
	}

	extraction, mediaType, byteLength, err := service.extraction(
		ctx, authorization.ResourceProjectID, bucket, name,
	)
	if err != nil {
		return RuntimeAttachmentObjectContext{}, err
	}
	if extraction.Refused {
		return RuntimeAttachmentObjectContext{}, unreadable(extraction.Reason)
	}
	return service.document(
		authorization.ResourceProjectID, bucket, name, mediaType, byteLength, extraction.Document,
	)
}

// extraction returns the sidecar if one is filed for the object as it is
// now, and extracts (and files) one otherwise.
//
// THE WAIT IS BOUNDED, ALL OF IT. A request waits at most extractionWait
// for an answer, and that includes the wait for an extraction slot: the
// slot is taken inside the extraction, not before it. When the wait ends
// first, the request answers "processing" and the extraction keeps running
// and files its result.
//
// ONE EXTRACTION PER OBJECT VERSION. Requests for the same object (a
// regenerate, two workers, one file in several turns at once) join the
// extraction that is already running instead of starting another one and
// taking another slot.
func (service *RuntimeAttachmentObjectService) extraction(
	ctx context.Context,
	projectID int64,
	bucket string,
	name string,
) (AttachmentExtraction, string, int64, error) {
	var version AttachmentObjectVersion
	cached := false
	if service.cache != nil {
		hit, objectVersion, found, err := service.cache.LoadAttachmentExtraction(
			ctx, projectID, bucket, name, extract.Version,
		)
		switch {
		case err == nil:
			version = objectVersion
			if found && !service.retryDue(hit) {
				return hit, objectVersion.MediaType, objectVersion.ByteLength, nil
			}
			cached = true
		case errors.Is(err, ErrContentNotFound):
			return AttachmentExtraction{}, "", 0, ErrContentNotFound
		case ctx.Err() != nil:
			return AttachmentExtraction{}, "", 0, ctx.Err()
		default:
			// The sidecar store is an optimisation. Reading the object
			// directly still gives the right answer.
			service.logger.WarnContext(ctx, "attachment extraction sidecar unavailable", "error", err)
		}
	}

	// The key names the object VERSION, so a re-upload under the same key
	// starts its own extraction rather than joining the old one.
	key := strings.Join([]string{
		strconv.FormatInt(projectID, 10), bucket, name,
		strconv.FormatInt(version.ObjectID, 10), strconv.FormatInt(version.UpdatedAt, 10),
		extract.Version,
	}, "\x00")
	detached := context.WithoutCancel(ctx)
	results := service.flights.DoChan(key, func() (any, error) {
		return service.extractDetached(detached, projectID, bucket, name, cached, version), nil
	})
	wait := time.NewTimer(service.extractionWait)
	defer wait.Stop()
	var result attachmentExtractionResult
	select {
	case shared := <-results:
		result, _ = shared.Val.(attachmentExtractionResult)
	case <-wait.C:
		if !cached {
			// Nothing would file the result, so a later turn could not use
			// it: wait for it instead.
			select {
			case shared := <-results:
				result, _ = shared.Val.(attachmentExtractionResult)
			case <-ctx.Done():
				return AttachmentExtraction{}, "", 0, ctx.Err()
			}
			break
		}
		return AttachmentExtraction{Refused: true, Reason: ReasonAttachmentProcessing}, version.MediaType, version.ByteLength, nil
	case <-ctx.Done():
		return AttachmentExtraction{}, "", 0, ctx.Err()
	}
	if result.err != nil {
		return AttachmentExtraction{}, "", 0, result.err
	}
	return result.outcome, result.mediaType, result.byteLength, nil
}

// ReasonAttachmentProcessing is the 422 reason for a file whose extraction
// is still running. It is not filed: the extraction files its own result.
const ReasonAttachmentProcessing extract.Reason = "processing"

// ReasonAttachmentUnreadable is the 422 reason for a rejection that named
// no reason. It claims nothing about the file it cannot back up.
const ReasonAttachmentUnreadable extract.Reason = "unreadable"

// attachmentTimeoutRetryAfter is how long a filed timeout answers for the
// object. A timeout depends on load as much as on the file, so it is tried
// again after this; within it, a file that always times out is not parsed
// again on every turn.
const attachmentTimeoutRetryAfter = 15 * time.Minute

// maxQueuedAttachmentExtractions bounds the extractions that wait for a
// slot. Past it a request is refused as unavailable at once rather than
// queued behind work that will not finish inside its wait.
const maxQueuedAttachmentExtractions = 16

// errAttachmentExtractionBusy is a full queue: a server condition, never a
// property of the file.
var errAttachmentExtractionBusy = errors.New("attachment extraction queue is full")

type attachmentExtractionResult struct {
	outcome    AttachmentExtraction
	mediaType  string
	byteLength int64
	err        error
}

// retryDue reports whether a filed timeout is old enough to try again.
func (service *RuntimeAttachmentObjectService) retryDue(hit AttachmentExtraction) bool {
	if !hit.Refused || hit.Reason != extract.ReasonTimeout {
		return false
	}
	return hit.ExtractedAt.IsZero() || service.now().Sub(hit.ExtractedAt) >= attachmentTimeoutRetryAfter
}

// extractDetached takes a slot, reads the object, and extracts and files it.
// Its context keeps the request's values but not its cancellation; the
// extractor's own time limit bounds it.
//
// The slot is taken BEFORE the object is read, so the source bytes held at
// once are bounded by the slots, not by the number of waiting requests.
func (service *RuntimeAttachmentObjectService) extractDetached(
	ctx context.Context,
	projectID int64,
	bucket string,
	name string,
	cached bool,
	version AttachmentObjectVersion,
) (result attachmentExtractionResult) {
	// It runs on its own goroutine, where a panic would end the process
	// (singleflight re-panics it). One attachment must not take the API
	// down: report the stage as unavailable instead.
	defer func() {
		if recovered := recover(); recovered != nil {
			service.logger.ErrorContext(ctx, "attachment extraction panicked", "panic", recovered)
			result = attachmentExtractionResult{err: runtimeContextUnavailable(runtimeContextStageAttachmentExtract)}
		}
	}()
	if service.queued.Add(1) > maxQueuedAttachmentExtractions {
		service.queued.Add(-1)
		service.logger.WarnContext(ctx, "attachment extraction queue is full")
		return attachmentExtractionResult{err: runtimeContextUnavailable(runtimeContextStageAttachmentExtract)}
	}
	select {
	case service.extractions <- struct{}{}:
		service.queued.Add(-1)
	case <-ctx.Done():
		service.queued.Add(-1)
		return attachmentExtractionResult{err: ctx.Err()}
	}
	defer func() { <-service.extractions }()

	record, err := service.objects.ReadAttachmentObject(ctx, projectID, bucket, name, service.maxBytes)
	if err != nil {
		switch {
		case errors.Is(err, ErrContentNotFound):
			return attachmentExtractionResult{err: ErrContentNotFound}
		case errors.Is(err, ErrContentRejected):
			outcome := AttachmentExtraction{Refused: true, Reason: ReasonAttachmentUnreadable}
			var refusal *AttachmentUnreadableError
			if errors.As(err, &refusal) && refusal.Reason != "" {
				outcome.Reason = refusal.Reason
			}
			return attachmentExtractionResult{outcome: outcome, mediaType: version.MediaType, byteLength: version.ByteLength}
		}
		service.logger.WarnContext(ctx, "attachment object read failed", "error", err)
		return attachmentExtractionResult{err: runtimeContextUnavailable(runtimeContextStageAttachmentRead)}
	}
	defer clearContentBytes(record.Content)
	// Repeated against what the URL asked for even though the source already
	// filtered on both. The worker validates the same pair on its side and
	// would reject a mismatched document as an authorization failure with no
	// diagnosis, so a disagreement is worth naming here instead.
	if record.Bucket != bucket || record.Name != name {
		return attachmentExtractionResult{err: ErrContentNotFound}
	}
	done := func(outcome AttachmentExtraction) attachmentExtractionResult {
		return attachmentExtractionResult{outcome: outcome, mediaType: record.MediaType, byteLength: record.ByteLength}
	}
	if int64(len(record.Content)) > service.maxBytes {
		return done(AttachmentExtraction{Refused: true, Reason: extract.ReasonTooLarge})
	}
	if record.ByteLength != int64(len(record.Content)) {
		// The metadata row and the bytes disagree, usually a read that
		// raced a re-upload. That is a server condition, and the next read
		// sees one version: it is not "too large" and it is not filed.
		service.logger.WarnContext(ctx, "attachment object length disagrees with its metadata row")
		return attachmentExtractionResult{err: runtimeContextUnavailable(runtimeContextStageAttachmentRead)}
	}

	// A byte-identical re-upload under the same key gets a new updated_at,
	// so the lookup above misses. The digest finds the extraction already
	// filed for these bytes and files it under the new version.
	var digest []byte
	if cached {
		sum := sha256.Sum256(record.Content)
		digest = sum[:]
		hit, found, err := service.cache.LoadAttachmentExtractionByDigest(ctx, version, extract.Version, digest)
		switch {
		case err != nil:
			service.logger.WarnContext(ctx, "attachment extraction digest lookup failed", "error", err)
		case found && !service.retryDue(hit):
			return done(hit)
		}
	}

	document, err := service.extractor.Extract(ctx, record.Content)
	var outcome AttachmentExtraction
	if err != nil {
		reason, refused := extract.ReasonOf(err)
		if !refused {
			service.logger.WarnContext(ctx, "attachment extractor unavailable", "error", err)
			return attachmentExtractionResult{err: runtimeContextUnavailable(runtimeContextStageAttachmentExtract)}
		}
		outcome = AttachmentExtraction{Refused: true, Reason: reason}
	} else {
		outcome = AttachmentExtraction{Document: document}
	}
	// A timeout is filed too, and answers for attachmentTimeoutRetryAfter:
	// it depends on load as well as on the file, so it is tried again
	// after that, but not on every turn before it.
	if cached {
		if err := service.cache.SaveAttachmentExtraction(ctx, version, digest, outcome); err != nil {
			service.logger.WarnContext(ctx, "attachment extraction could not be filed", "error", err)
		}
	}
	return done(outcome)
}

// document projects an extraction onto the wire, serving at most maxServed
// bytes of text and cutting only at a unit boundary when one fits.
func (service *RuntimeAttachmentObjectService) document(
	projectID int64,
	bucket string,
	name string,
	mediaType string,
	byteLength int64,
	document extract.Document,
) (RuntimeAttachmentObjectContext, error) {
	text := document.Text
	if text == "" || !utf8.ValidString(text) {
		return RuntimeAttachmentObjectContext{}, unreadable(extract.ReasonNoText)
	}
	complete := !document.Partial
	partialReason := string(document.PartialBy)
	cut := len(text)
	if cut > service.maxServed {
		complete = false
		partialReason = "served_limit"
		cut = 0
		for _, unit := range document.Units {
			if unit.End <= service.maxServed {
				cut = unit.End
			}
		}
		if cut == 0 {
			// One unit alone is longer than a response: serve its start, at
			// a line end when there is one.
			cut = service.maxServed
			if newline := strings.LastIndexByte(text[:cut], '\n'); newline > cut/2 {
				cut = newline
			}
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
	}
	units := make([]RuntimeAttachmentUnit, 0, len(document.Units))
	for _, unit := range document.Units {
		if unit.Start >= cut && cut < len(text) {
			break
		}
		end := unit.End
		if end > cut {
			end = cut
		}
		units = append(units, RuntimeAttachmentUnit{
			Kind:    string(unit.Kind),
			Label:   unit.Label,
			Start:   unit.Start,
			End:     end,
			HasText: unit.HasText,
		})
	}
	lowText := make([]int, 0, len(document.LowTextUnits))
	for _, number := range document.LowTextUnits {
		if number <= len(units) {
			lowText = append(lowText, number)
		}
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return RuntimeAttachmentObjectContext{
		SchemaVersion:    RuntimeAttachmentObjectSchemaVersion,
		ProjectID:        projectID,
		Bucket:           bucket,
		Name:             name,
		MediaType:        mediaType,
		ByteLength:       byteLength,
		Format:           string(document.Format),
		ExtractorVersion: document.ExtractorVersion,
		Content:          text[:cut],
		Units:            units,
		UnitCount:        max(document.UnitCount, len(units)),
		LowTextUnits:     lowText,
		TextBytes:        int64(len(text)),
		TokenEstimate:    document.TokenEstimate,
		Complete:         complete,
		PartialReason:    partialReason,
	}, nil
}

// conversationScopedAttachmentKey is the cross-conversation refusal.
//
// `name` is the object KEY, conversation prefix included — the upload endpoint
// keys every chat attachment `{conversationUUID}/{sanitised filename}` and the
// admission path refuses any reference without that prefix. This restates the
// same test against the conversation the CLAIM resolved, so the two ends cannot
// drift into disagreeing about which files a turn may read.
//
// The trailing segment must be non-empty: `{uuid}/` addresses no object, and
// admitting it would turn the prefix test into a bare "starts with a uuid".
func conversationScopedAttachmentKey(conversationID, name string) bool {
	prefix := conversationID + "/"
	return strings.HasPrefix(name, prefix) && len(name) > len(prefix)
}

// canonicalConversationIdentity accepts only a lowercase canonical UUID.
//
// Shape, not merely non-emptiness: this value becomes an authorization PREFIX,
// and a value that could be empty or could be a bare "1" would make the prefix
// test match keys it was never meant to. `chat_conversations.uuid` is a real
// uuid column and Postgres renders it lowercase-canonical, so nothing
// legitimate is excluded.
func canonicalConversationIdentity(value string) bool {
	if len(value) != canonicalUUIDLength {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		hexadecimal := (character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f')
		if !hexadecimal {
			return false
		}
	}
	return true
}

// addressableAttachmentBucket / addressableAttachmentKey restate the rules
// NewObjectRef's own bucketPattern and validateKey apply, and the extra ones
// internal/application/agentexecution/attachments.go applies on the way in.
//
// They are restated rather than delegated because this is the REFUSAL boundary:
// a name that reaches NewObjectRef and fails there is an internal error at the
// bottom of a stack, while one refused here is a plain 404 with the claim never
// spent. Length and control characters are already checked by the route's own
// claimPathPart before either is called.
func addressableAttachmentBucket(bucket string) bool {
	if len(bucket) < 2 || len(bucket) > 63 {
		return false
	}
	if bucket[0] < 'a' || bucket[0] > 'z' {
		return false
	}
	for index := 1; index < len(bucket); index++ {
		character := bucket[index]
		valid := (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '-'
		if !valid {
			return false
		}
	}
	return true
}

func addressableAttachmentKey(key string) bool {
	if key == "" || len(key) > maxAttachmentReferenceBytes ||
		strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") ||
		strings.Contains(key, "//") {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// maxAttachmentReferenceBytes is varchar(256) in migrations/tenant/0127, which
// is also what the admission path refuses beyond (maxAttachmentFieldBytes) and
// what the worker refuses beyond (MAX_ATTACHMENT_FIELD_BYTES). A reference
// longer than this could not have been stored, so serving it would mean
// serving something no attachment row can name.
const maxAttachmentReferenceBytes = 256
