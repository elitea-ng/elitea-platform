package storage

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The `artifact` toolkit family on the NATIVE runtime (#906).
//
// The four routes served from here — list, read, write, delete — are the
// native worker's only channel to this project's artifact buckets, and they
// exist for the same reason the attachment READ route and the two builder
// WRITE routes do (see runtime_attachment_object.go and
// runtime_entity_builder.go): the native runtime holds no vault, redeems no
// artifact credential, and its egress allowlist reaches the model gateway
// alone. Before them, an `artifact` toolkit attached to an agent was SKIPPED at
// assembly (`agent_toolkit_skipped reason_code=unsupported_toolkit_family`),
// so the toggle the user turned on did nothing and — for an agent whose only
// tool was that one — sometimes failed the turn outright.
//
// WHAT AUTHORIZES A CALL, and why it is not what the request carries:
//
//   - WHICH CLAIM. The authorizer is the AGENT-scoped one, so an
//     `index.ingest.v1` claim — which carries a perfectly real
//     resource_project_id — is refused before any lookup happens. An index
//     workload has no business writing a user's bucket.
//   - WHICH PROJECT. Taken from the claimed execution row, never from the
//     request. `storage.ObjectRef` is project-scoped by construction, so
//     another tenant's bytes are not addressable from here at all.
//   - WHICH BUCKET, FOR WHICH PERSON. The per-bucket access list
//     (`elitea_storage.bucket_permissions`, the surface
//     internal/api/v2/artifacts/bucket_permissions.go serves) is applied
//     against the claim's own ACTOR — the user the turn runs as — with the
//     same read/write verbs and the same project-admin bypass the HTTP routes
//     apply. A turn therefore reaches exactly the buckets the person who
//     started it reaches, no more: an agent is not a way around an exception
//     an administrator wrote about that person.
//
// The REQUEST selects only the bucket name, the key and (for a write) the
// bytes, and it selects them INSIDE that project and that access list.
const (
	// The exact discriminators the worker compares before it will read a body
	// at all (ARTIFACT_*_SCHEMA,
	// services/elitea-worker-rust/src/transport/runtime_context.rs).
	RuntimeArtifactListSchemaVersion   = "elitea.runtime.artifact-list.v1"
	RuntimeArtifactReadSchemaVersion   = "elitea.runtime.artifact-read.v1"
	RuntimeArtifactWriteSchemaVersion  = "elitea.runtime.artifact-write.v1"
	RuntimeArtifactDeleteSchemaVersion = "elitea.runtime.artifact-delete.v1"

	// maxRuntimeArtifactReadChars is the AGENT-PATH cap, in characters, and it
	// is deliberately the SDK worker's own number
	// (DEFAULT_MAX_OUTPUT_CHARS = 200_000,
	// elitea_sdk/tools/utils/file_metadata.py). The two runtimes must refuse
	// the same file: a cap that differed by runtime would make "can this agent
	// read this file" depend on which worker image answered, which is exactly
	// the class of difference ELITEA-0362/0364 were written to pin down.
	//
	// It is a CHARACTER count because that is what the SDK measures and what
	// the refusal names; bytes would refuse a UTF-8-heavy file the SDK admits.
	maxRuntimeArtifactReadChars = 200_000

	// maxRuntimeArtifactReadBytes is the byte ceiling the SOURCE is allowed to
	// fetch, and it exists so an oversized object is never fully read just to
	// discover it is oversized. One UTF-8 character is at most four bytes, so
	// an object above this cannot possibly be within the character cap and is
	// refused from its metadata row alone, without touching object storage.
	maxRuntimeArtifactReadBytes = 4 * maxRuntimeArtifactReadChars

	// maxRuntimeArtifactWriteChars bounds what a MODEL may write in one call.
	// It is smaller than the read cap on purpose: a read serves a file a human
	// put there, while a write is a document the model composed inside one
	// turn, and 64k characters is already more than any turn can afford to
	// produce.
	maxRuntimeArtifactWriteChars = 64 * 1024

	// maxRuntimeArtifactRequestBytes bounds the four request BODIES. Only the
	// write carries content; the ceiling is the write cap plus room for the
	// JSON envelope and worst-case escaping of the content string.
	maxRuntimeArtifactRequestBytes = 512 * 1024

	// maxRuntimeArtifactResponseBytes is the envelope ceiling, restated on this
	// side so main REFUSES rather than sends a body the worker will reject
	// after buffering it (MAX_ARTIFACT_RESPONSE_BYTES on the worker). It is
	// larger than the character cap because the content travels as a JSON
	// STRING: a file made of control characters escapes to six characters per
	// byte.
	maxRuntimeArtifactResponseBytes = 2 * 1024 * 1024

	// maxRuntimeArtifactListFiles bounds one listing page. The SDK's own
	// listing is unbounded, which is fine for a page a human scrolls and not
	// for a value spliced into a prompt.
	maxRuntimeArtifactListFiles = 500

	maxRuntimeArtifactPrefixBytes = 1024

	runtimeContextStageArtifactList   = "artifact_list"
	runtimeContextStageArtifactRead   = "artifact_read"
	runtimeContextStageArtifactWrite  = "artifact_write"
	runtimeContextStageArtifactDelete = "artifact_delete"
)

// RuntimeArtifactListRequest is the wire body for `list_files`.
//
// `Bucket` is optional: the toolkit is configured with one bucket and the
// worker sends that one, but the SDK's tools all accept a `bucket_name`
// override, so the field exists here to carry it. Whatever arrives is still
// resolved inside the claim's project and still checked against the actor's
// access list — an override widens nothing.
type RuntimeArtifactListRequest struct {
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Recursive bool   `json:"recursive"`
	Limit     int32  `json:"limit"`
}

// RuntimeArtifactReadRequest is the wire body for `read_file`.
type RuntimeArtifactReadRequest struct {
	Bucket string `json:"bucket"`
	Name   string `json:"name"`
}

// RuntimeArtifactWriteRequest is the wire body for `create_file`.
type RuntimeArtifactWriteRequest struct {
	Bucket  string `json:"bucket"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// RuntimeArtifactDeleteRequest is the wire body for `delete_file`.
type RuntimeArtifactDeleteRequest struct {
	Bucket string `json:"bucket"`
	Name   string `json:"name"`
}

// RuntimeArtifactFile is one listed object.
type RuntimeArtifactFile struct {
	Name       string `json:"name"`
	ByteLength int64  `json:"byte_length"`
	MediaType  string `json:"media_type"`
	ModifiedAt string `json:"modified_at"`
}

// RuntimeArtifactListContext, …ReadContext, …WriteContext and …DeleteContext
// are the response documents. Their fields are the complete set the worker
// accepts — every one of its response types is `deny_unknown_fields` — so one
// extra key here fails the call with a malformed-response error that names
// nothing.
type RuntimeArtifactListContext struct {
	SchemaVersion string                `json:"schema_version"`
	ProjectID     int64                 `json:"project_id"`
	Bucket        string                `json:"bucket"`
	Files         []RuntimeArtifactFile `json:"files"`
	Truncated     bool                  `json:"truncated"`
}

// RuntimeArtifactReadContext carries the file's TEXT, or — when the file is
// over the agent-path cap — the measurement that refuses it.
//
// The refusal travels as a 200 with `over_limit: true` rather than as a 422,
// and that is deliberate. A 422 would tell the worker only "rejected", and the
// tool's answer to the model has to NAME the cap and the actual size or the
// model cannot choose a slice that would fit — which is the whole difference
// between the SDK's structured `content_too_large` result and the bare string
// it replaced. `CharLength` is 0 when the object was too large to measure
// without reading it (see maxRuntimeArtifactReadBytes); the worker then reports
// the byte length instead of inventing a character count.
type RuntimeArtifactReadContext struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	Bucket        string `json:"bucket"`
	Name          string `json:"name"`
	MediaType     string `json:"media_type"`
	ByteLength    int64  `json:"byte_length"`
	CharLength    int64  `json:"char_length"`
	TotalLines    int64  `json:"total_lines"`
	MaxChars      int64  `json:"max_chars"`
	OverLimit     bool   `json:"over_limit"`
	Content       string `json:"content"`
}

type RuntimeArtifactWriteContext struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	Bucket        string `json:"bucket"`
	Name          string `json:"name"`
	MediaType     string `json:"media_type"`
	ByteLength    int64  `json:"byte_length"`
}

type RuntimeArtifactDeleteContext struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	Bucket        string `json:"bucket"`
	Name          string `json:"name"`
	Deleted       bool   `json:"deleted"`
}

// RuntimeArtifactRecord is one object's metadata as the platform records it.
type RuntimeArtifactRecord struct {
	Name       string
	MediaType  string
	ByteLength int64
	ModifiedAt time.Time
}

// RuntimeArtifactContentRecord is one object's bytes, or the measurement that
// says they were never fetched.
//
// OverLimit is set by the SOURCE when the metadata row already says the object
// is past the caller's byte ceiling. Content is then empty and ByteLength is
// the row's — the point of the flag is that object storage was not touched.
type RuntimeArtifactContentRecord struct {
	Name       string
	MediaType  string
	ByteLength int64
	Content    []byte
	OverLimit  bool
}

// RuntimeArtifactSource is the claim-scoped artifact plane.
//
// Every method takes the project AND the actor the claim resolved, and no
// implementation may accept either from the request. The actor is not
// decoration: it is the subject of the per-bucket access list, and an
// implementation that ignored it would make every agent turn a way around an
// exception written about the person who started it.
type RuntimeArtifactSource interface {
	ListRuntimeArtifacts(
		ctx context.Context,
		projectID int64,
		actorID int64,
		bucket string,
		prefix string,
		recursive bool,
		limit int32,
	) ([]RuntimeArtifactRecord, bool, error)
	ReadRuntimeArtifact(
		ctx context.Context,
		projectID int64,
		actorID int64,
		bucket string,
		name string,
		maxBytes int64,
	) (RuntimeArtifactContentRecord, error)
	WriteRuntimeArtifact(
		ctx context.Context,
		projectID int64,
		actorID int64,
		bucket string,
		name string,
		content []byte,
	) (RuntimeArtifactRecord, error)
	DeleteRuntimeArtifact(
		ctx context.Context,
		projectID int64,
		actorID int64,
		bucket string,
		name string,
	) error
}

// RuntimeArtifactObjectService serves all four artifact operations under one
// durable claim.
//
// They live on one service rather than four because they share the whole
// authorization path — claim, project, actor — and differ only in what they do
// afterwards, exactly the argument RuntimeEntityBuilderService makes for its
// two writes.
type RuntimeArtifactObjectService struct {
	authorizer AgentRuntimeContextAuthorizer
	artifacts  RuntimeArtifactSource
}

func NewRuntimeArtifactObjectService(
	authorizer AgentRuntimeContextAuthorizer,
	artifacts RuntimeArtifactSource,
) (*RuntimeArtifactObjectService, error) {
	if authorizer == nil || artifacts == nil {
		return nil, errors.New("runtime artifact dependencies are required")
	}
	return &RuntimeArtifactObjectService{authorizer: authorizer, artifacts: artifacts}, nil
}

// runtimeArtifactPrincipal is what one authorized claim resolves to, and it is
// the only thing any of the four operations is allowed to act on.
type runtimeArtifactPrincipal struct {
	projectID int64
	actorID   int64
}

// authorize runs the shared half of all four calls: the agent-scoped claim
// check, the project identity, and the actor identity the access list is
// about. It returns nothing else, so no caller can accidentally read a
// request-supplied value in place of one of them.
func (service *RuntimeArtifactObjectService) authorize(
	ctx context.Context,
	claim ContentClaim,
	stage string,
) (runtimeArtifactPrincipal, error) {
	if service == nil || service.authorizer == nil || service.artifacts == nil {
		return runtimeArtifactPrincipal{}, runtimeContextUnavailable(stage)
	}
	if err := ctx.Err(); err != nil {
		return runtimeArtifactPrincipal{}, err
	}
	authorization, err := service.authorizer.AuthorizeAgentRuntimeContext(ctx, claim)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return runtimeArtifactPrincipal{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) {
			return runtimeArtifactPrincipal{}, ErrContentUnauthorized
		}
		return runtimeArtifactPrincipal{}, runtimeContextUnavailable(
			runtimeContextStageClaimAuthorize,
		)
	}
	if authorization.ResourceProjectID <= 0 || authorization.ResourceProjectID > math.MaxInt32 {
		return runtimeArtifactPrincipal{}, runtimeContextUnavailable(
			runtimeContextStageProjectIdentity,
		)
	}
	// The actor is REQUIRED, not best-effort. A claim with no usable actor is
	// an UNAVAILABLE rather than a refusal — every execution row carries one —
	// and serving the call without it would mean applying no access list at
	// all, which is worse than failing.
	actorID, err := strconv.ParseInt(authorization.ActorID, 10, 64)
	if err != nil || actorID <= 0 || actorID > math.MaxInt32 ||
		strconv.FormatInt(actorID, 10) != authorization.ActorID {
		return runtimeArtifactPrincipal{}, runtimeContextUnavailable(
			runtimeContextStageExecutionActor,
		)
	}
	return runtimeArtifactPrincipal{projectID: authorization.ResourceProjectID, actorID: actorID}, nil
}

// List serves one bucket listing for the claimed execution.
func (service *RuntimeArtifactObjectService) List(
	ctx context.Context,
	claim ContentClaim,
	request RuntimeArtifactListRequest,
) (RuntimeArtifactListContext, error) {
	bucket := strings.TrimSpace(request.Bucket)
	prefix := strings.TrimPrefix(strings.TrimSpace(request.Prefix), "/")
	if !addressableAttachmentBucket(bucket) || !addressableArtifactPrefix(prefix) {
		return RuntimeArtifactListContext{}, ErrContentRejected
	}
	limit := int32(maxRuntimeArtifactListFiles)
	if request.Limit > 0 && request.Limit < limit {
		limit = request.Limit
	}
	principal, err := service.authorize(ctx, claim, runtimeContextStageArtifactList)
	if err != nil {
		return RuntimeArtifactListContext{}, err
	}
	records, truncated, err := service.artifacts.ListRuntimeArtifacts(
		ctx, principal.projectID, principal.actorID, bucket, prefix, request.Recursive, limit,
	)
	if err != nil {
		return RuntimeArtifactListContext{}, service.sourceError(ctx, err, runtimeContextStageArtifactList)
	}
	files := make([]RuntimeArtifactFile, 0, len(records))
	for _, record := range records {
		files = append(files, RuntimeArtifactFile{
			Name:       record.Name,
			ByteLength: record.ByteLength,
			MediaType:  record.MediaType,
			ModifiedAt: record.ModifiedAt.UTC().Format(time.RFC3339),
		})
	}
	return RuntimeArtifactListContext{
		SchemaVersion: RuntimeArtifactListSchemaVersion,
		ProjectID:     principal.projectID,
		Bucket:        bucket,
		Files:         files,
		Truncated:     truncated,
	}, nil
}

// Read serves one object's TEXT, or the measurement that refuses it for size.
//
// THE CONTENT DISCIPLINE is the attachment route's, for the same reason: a
// file that is not UTF-8 has no text to put in a prompt, and an empty
// "content" a model is told is the file reads as an empty file. Both are 422,
// which the worker turns into a tool result the model can act on rather than a
// failed turn.
func (service *RuntimeArtifactObjectService) Read(
	ctx context.Context,
	claim ContentClaim,
	request RuntimeArtifactReadRequest,
) (RuntimeArtifactReadContext, error) {
	bucket := strings.TrimSpace(request.Bucket)
	name := strings.TrimPrefix(strings.TrimSpace(request.Name), "/")
	if !addressableAttachmentBucket(bucket) || !addressableAttachmentKey(name) {
		return RuntimeArtifactReadContext{}, ErrContentNotFound
	}
	principal, err := service.authorize(ctx, claim, runtimeContextStageArtifactRead)
	if err != nil {
		return RuntimeArtifactReadContext{}, err
	}
	record, err := service.artifacts.ReadRuntimeArtifact(
		ctx, principal.projectID, principal.actorID, bucket, name, maxRuntimeArtifactReadBytes,
	)
	if err != nil {
		return RuntimeArtifactReadContext{}, service.sourceError(ctx, err, runtimeContextStageArtifactRead)
	}
	if record.Name != name {
		return RuntimeArtifactReadContext{}, ErrContentNotFound
	}
	answer := RuntimeArtifactReadContext{
		SchemaVersion: RuntimeArtifactReadSchemaVersion,
		ProjectID:     principal.projectID,
		Bucket:        bucket,
		Name:          record.Name,
		MediaType:     record.MediaType,
		ByteLength:    record.ByteLength,
		MaxChars:      maxRuntimeArtifactReadChars,
	}
	if record.OverLimit {
		// Measured from the metadata row alone: the bytes were never fetched,
		// so there is no character count and no line count to report. Saying 0
		// is the honest answer; the worker reports the byte length instead.
		answer.OverLimit = true
		return answer, nil
	}
	if len(record.Content) == 0 || !utf8.Valid(record.Content) {
		return RuntimeArtifactReadContext{}, ErrContentRejected
	}
	content := string(record.Content)
	characters := int64(utf8.RuneCountInString(content))
	answer.CharLength = characters
	answer.TotalLines = int64(strings.Count(content, "\n") + 1)
	if characters > maxRuntimeArtifactReadChars {
		// REFUSED, never truncated. A prefix presented as the file is worse
		// than a refusal that names the cap: the model answers as though it
		// read the whole thing.
		answer.OverLimit = true
		return answer, nil
	}
	answer.Content = content
	return answer, nil
}

// Write creates or replaces one object.
//
// The content is TEXT for the same reason the read serves text: the only
// producer here is a model, and there is no binary a model composes. The media
// type is NOT taken from the request — it is derived from the key's extension
// by the same rule the upload routes apply, so a `.md` written by an agent is
// served to a browser exactly as one uploaded by a person.
func (service *RuntimeArtifactObjectService) Write(
	ctx context.Context,
	claim ContentClaim,
	request RuntimeArtifactWriteRequest,
) (RuntimeArtifactWriteContext, error) {
	bucket := strings.TrimSpace(request.Bucket)
	name := strings.TrimPrefix(strings.TrimSpace(request.Name), "/")
	content := request.Content
	if !addressableAttachmentBucket(bucket) || !addressableAttachmentKey(name) {
		return RuntimeArtifactWriteContext{}, ErrContentRejected
	}
	if content == "" || !utf8.ValidString(content) ||
		strings.ContainsRune(content, 0) ||
		utf8.RuneCountInString(content) > maxRuntimeArtifactWriteChars {
		return RuntimeArtifactWriteContext{}, ErrContentRejected
	}
	principal, err := service.authorize(ctx, claim, runtimeContextStageArtifactWrite)
	if err != nil {
		return RuntimeArtifactWriteContext{}, err
	}
	record, err := service.artifacts.WriteRuntimeArtifact(
		ctx, principal.projectID, principal.actorID, bucket, name, []byte(content),
	)
	if err != nil {
		return RuntimeArtifactWriteContext{}, service.sourceError(ctx, err, runtimeContextStageArtifactWrite)
	}
	return RuntimeArtifactWriteContext{
		SchemaVersion: RuntimeArtifactWriteSchemaVersion,
		ProjectID:     principal.projectID,
		Bucket:        bucket,
		Name:          record.Name,
		MediaType:     record.MediaType,
		ByteLength:    record.ByteLength,
	}, nil
}

// Delete removes one object and its metadata row.
func (service *RuntimeArtifactObjectService) Delete(
	ctx context.Context,
	claim ContentClaim,
	request RuntimeArtifactDeleteRequest,
) (RuntimeArtifactDeleteContext, error) {
	bucket := strings.TrimSpace(request.Bucket)
	name := strings.TrimPrefix(strings.TrimSpace(request.Name), "/")
	if !addressableAttachmentBucket(bucket) || !addressableAttachmentKey(name) {
		return RuntimeArtifactDeleteContext{}, ErrContentNotFound
	}
	principal, err := service.authorize(ctx, claim, runtimeContextStageArtifactDelete)
	if err != nil {
		return RuntimeArtifactDeleteContext{}, err
	}
	if err := service.artifacts.DeleteRuntimeArtifact(
		ctx, principal.projectID, principal.actorID, bucket, name,
	); err != nil {
		return RuntimeArtifactDeleteContext{}, service.sourceError(ctx, err, runtimeContextStageArtifactDelete)
	}
	return RuntimeArtifactDeleteContext{
		SchemaVersion: RuntimeArtifactDeleteSchemaVersion,
		ProjectID:     principal.projectID,
		Bucket:        bucket,
		Name:          name,
		Deleted:       true,
	}, nil
}

// sourceError maps one source failure onto the listener's shared taxonomy.
//
// The three classified causes are the three a caller can act on differently: a
// refused bucket (403) says the person this turn runs as may not touch it, a
// missing object (404) says the reference is stale, and a rejected one (422)
// says the file is exactly what it claims to be and cannot be served as text.
// Anything else is an unavailability with a named stage, because an operator
// has to be able to tell a broken dependency from a caller's mistake.
func (service *RuntimeArtifactObjectService) sourceError(
	ctx context.Context,
	err error,
	stage string,
) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	switch {
	case errors.Is(err, ErrContentUnauthorized):
		return ErrContentUnauthorized
	case errors.Is(err, ErrContentNotFound):
		return ErrContentNotFound
	case errors.Is(err, ErrContentRejected):
		return ErrContentRejected
	}
	return runtimeContextUnavailable(stage)
}

// addressableArtifactPrefix restates storage.ValidateKeyPrefix's rules at the
// REFUSAL boundary, for the reason addressableAttachmentKey gives: a prefix
// that reaches the store and fails there is an internal error at the bottom of
// a stack, while one refused here is a plain 422 with the claim never spent.
// The empty prefix is the whole bucket and is allowed.
func addressableArtifactPrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	if len(prefix) > maxRuntimeArtifactPrefixBytes ||
		!utf8.ValidString(prefix) ||
		strings.Contains(prefix, "//") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	for _, character := range prefix {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
