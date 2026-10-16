package workspacestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// PhysicalStore is one physical bucket of the deployment's object store.
// Name is recorded on every segment row written to it, so a reader always
// opens the bucket the segment was written to.
type PhysicalStore struct {
	Name  string
	Store storage.ObjectStore
}

// Buckets are the physical buckets the workspace bucket can live in: the
// deployment's ADR-0016 bucket, and the optional dedicated one
// (`storage.workspace.bucket`, decision 3). New segments go to the dedicated
// bucket when it is set.
type Buckets struct {
	Default   PhysicalStore
	Dedicated *PhysicalStore
}

func (b Buckets) write() PhysicalStore {
	if b.Dedicated != nil && b.Dedicated.Store != nil {
		return *b.Dedicated
	}
	return b.Default
}

func (b Buckets) open(name string) (storage.ObjectStore, bool) {
	if b.Dedicated != nil && b.Dedicated.Store != nil && name == b.Dedicated.Name {
		return b.Dedicated.Store, true
	}
	if b.Default.Store != nil && name == b.Default.Name {
		return b.Default.Store, true
	}
	return nil, false
}

func (b Buckets) all() []PhysicalStore {
	out := []PhysicalStore{}
	if b.Default.Store != nil {
		out = append(out, b.Default)
	}
	if b.Dedicated != nil && b.Dedicated.Store != nil && b.Dedicated.Name != b.Default.Name {
		out = append(out, *b.Dedicated)
	}
	return out
}

// LimitsSource answers a project's effective caps (the platform ceiling
// narrowed by the project setting).
type LimitsSource interface {
	Limits(ctx context.Context, projectID int64) (Limits, error)
}

// The desktop routes of the streaming facade and the multipart completion
// (internal/api/v2/workspacehandoff). The store answers them as URLs where a
// backend cannot presign, so the client never needs to know the difference.
const (
	ObjectRoutePrefix         = "/api/v2/elitea_core/workspace_store_object/prompt_lib/"
	UploadCompleteRoutePrefix = "/api/v2/elitea_core/workspace_store_upload_complete/prompt_lib/"
)

// Service is the workspace store use case.
type Service struct {
	repo    Repository
	buckets Buckets
	limits  LimitsSource
	audit   Audit
	now     func() time.Time
	newID   func() (string, error)
	logger  *slog.Logger

	mu           sync.Mutex
	orphanCursor int64
	names        sync.Map
}

// NewService builds the store. Every dependency is required.
func NewService(repo Repository, buckets Buckets, limits LimitsSource, audit Audit, logger *slog.Logger) (*Service, error) {
	if repo == nil || buckets.Default.Store == nil || buckets.Default.Name == "" || limits == nil || audit == nil {
		return nil, errors.New("workspace store dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repo: repo, buckets: buckets, limits: limits, audit: audit,
		now: time.Now, newID: newGrantID, logger: logger,
	}, nil
}

// WithClock replaces the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// bucketName answers the project's workspace bucket name: found by TYPE
// (repo), cached per project because a bucket's name never changes.
// WorkspaceBucketName when the project has no workspace bucket yet (then no
// object can exist under it).
func (s *Service) bucketName(ctx context.Context, projectID int64) (string, error) {
	if cached, ok := s.names.Load(projectID); ok {
		return cached.(string), nil
	}
	bucket, ok, err := s.repo.WorkspaceBucket(ctx, projectID)
	if err != nil {
		return "", err
	}
	if !ok {
		return storage.WorkspaceBucketName, nil
	}
	s.names.Store(projectID, bucket.Name)
	return bucket.Name, nil
}

// ensureBucket creates the project's workspace bucket on first use.
func (s *Service) ensureBucket(ctx context.Context, projectID int64) (WorkspaceBucket, error) {
	bucket, err := s.repo.EnsureWorkspaceBucket(ctx, projectID)
	if err != nil {
		return WorkspaceBucket{}, err
	}
	s.names.Store(projectID, bucket.Name)
	return bucket, nil
}

func (s *Service) objectRef(ctx context.Context, projectID int64, key string) (storage.ObjectRef, error) {
	name, err := s.bucketName(ctx, projectID)
	if err != nil {
		return storage.ObjectRef{}, err
	}
	return storage.NewObjectRef(strconv.FormatInt(projectID, 10), name, key)
}

// requireWriter loads the thread and checks the caller may write in it at
// this epoch.
func (s *Service) requireWriter(ctx context.Context, thread Thread, caller Caller, epoch int64) (ThreadRow, error) {
	row, err := s.repo.ReadThread(ctx, thread)
	if err != nil {
		return ThreadRow{}, err
	}
	if caller.Writer == WriterDesktop && row.OwnerUserID != caller.UserID {
		return ThreadRow{}, ErrNotFound
	}
	if row.State != WriterState(caller.Writer) || row.Epoch != epoch {
		return ThreadRow{}, ErrStaleEpoch
	}
	if caller.Writer == WriterCloudExecutor && row.RunningExecutionID != caller.ExecutionID {
		return ThreadRow{}, ErrStaleEpoch
	}
	return row, nil
}

// UploadRequest asks for an upload grant for one segment.
type UploadRequest struct {
	Thread Thread
	Caller Caller
	Epoch  int64
	SHA256 string
	Bytes  int64
}

// Multipart is a native multipart upload: PUT each part to its URL in order,
// then POST the part ETags to CompleteURL.
type Multipart struct {
	UploadID    string
	PartSize    int64
	PartURLs    []string
	CompleteURL string
}

// UploadGrant answers an upload request.
type UploadGrant struct {
	Exists    bool
	PutURL    string
	Headers   map[string]string
	Multipart *Multipart
	ExpiresAt time.Time
}

// RequestUpload issues an upload grant for an exact segment key, valid for at
// most GrantTTL (decision 3). A digest already stored for the thread answers
// Exists and no URL.
func (s *Service) RequestUpload(ctx context.Context, request UploadRequest) (UploadGrant, error) {
	if !validThread(request.Thread) || !validCaller(request.Caller) || request.Epoch <= 0 ||
		!ValidSHA256(request.SHA256) || request.Bytes <= 0 {
		return UploadGrant{}, ErrInvalid
	}
	if _, err := s.requireWriter(ctx, request.Thread, request.Caller, request.Epoch); err != nil {
		return UploadGrant{}, err
	}
	limits, err := s.limits.Limits(ctx, request.Thread.ProjectID)
	if err != nil {
		return UploadGrant{}, fmt.Errorf("workspace store: limits: %w", err)
	}
	if request.Bytes > limits.MaxSegmentBytes {
		return UploadGrant{}, ErrSegmentTooLarge
	}
	if _, stored, err := s.repo.LiveSegment(ctx, request.Thread, request.SHA256); err != nil {
		return UploadGrant{}, err
	} else if stored {
		return UploadGrant{Exists: true, ExpiresAt: s.now().Add(GrantTTL).UTC()}, nil
	}
	if err := s.checkQuota(ctx, request.Thread, request.Bytes, limits); err != nil {
		return UploadGrant{}, err
	}
	bucket, err := s.ensureBucket(ctx, request.Thread.ProjectID)
	if err != nil {
		return UploadGrant{}, err
	}
	bucketID := bucket.ID
	key := ObjectKey(request.Thread.ConversationUUID, request.SHA256)
	ref, err := s.objectRef(ctx, request.Thread.ProjectID, key)
	if err != nil {
		return UploadGrant{}, ErrInvalid
	}
	target := s.buckets.write()
	grantID, err := s.newID()
	if err != nil {
		return UploadGrant{}, err
	}
	digest, _ := hex.DecodeString(request.SHA256)
	expires := s.now().Add(GrantTTL).UTC()
	answer := UploadGrant{ExpiresAt: expires}
	var uploadID *string
	capabilities := target.Store.Capabilities()
	switch {
	// A backend that verifies a SHA-256 checksum on a single PUT gets one
	// checksum-bound PUT up to its single-PUT ceiling: the digest is bound
	// here, and publish reads it back with HEAD instead of the bytes. A
	// multipart upload has only a composite checksum, so it is verified by
	// streaming at publish.
	case request.Bytes > MultipartThreshold && capabilities.NativeMultipart &&
		(!capabilities.ChecksumSHA256 || request.Bytes > SinglePutMaxBytes):
		id, err := target.Store.StartMultipart(ctx, ref, storage.PutOptions{ContentType: MediaType, ContentLength: -1})
		if err != nil {
			return UploadGrant{}, fmt.Errorf("workspace store: start multipart: %w", err)
		}
		value := string(id)
		uploadID = &value
		parts := (request.Bytes + PartSize - 1) / PartSize
		urls := make([]string, 0, parts)
		for part := int32(1); int64(part) <= parts; part++ {
			url, err := target.Store.PresignPart(ctx, ref, id, part, GrantTTL)
			if err != nil {
				_ = target.Store.AbortMultipart(ctx, ref, id)
				return UploadGrant{}, fmt.Errorf("workspace store: presign part: %w", err)
			}
			urls = append(urls, url)
		}
		answer.Multipart = &Multipart{
			UploadID: value, PartSize: PartSize, PartURLs: urls,
			CompleteURL: UploadCompleteRoutePrefix + strconv.FormatInt(request.Thread.ProjectID, 10) + "/" + grantID,
		}
	case capabilities.Presign:
		options := storage.PutOptions{ContentType: MediaType, ContentLength: request.Bytes}
		answer.Headers = map[string]string{"Content-Type": MediaType}
		if capabilities.ChecksumSHA256 {
			options.ChecksumSHA256 = digest
			answer.Headers[storage.ChecksumSHA256Header] = base64.StdEncoding.EncodeToString(digest)
		}
		url, err := target.Store.PresignPut(ctx, ref, GrantTTL, options)
		if err != nil {
			return UploadGrant{}, fmt.Errorf("workspace store: presign put: %w", err)
		}
		answer.PutURL = url
	default:
		// The ADR-0016 streaming facade: the client PUTs the bytes to Main,
		// which streams them to the store under the same grant.
		answer.PutURL = ObjectRoutePrefix + strconv.FormatInt(request.Thread.ProjectID, 10) + "/" +
			request.Thread.ConversationUUID + "/" + request.SHA256
		answer.Headers = map[string]string{"Content-Type": MediaType}
	}
	if err := s.repo.CreateGrant(ctx, GrantRecord{
		ID: grantID, ProjectID: request.Thread.ProjectID, BucketID: bucketID, Key: key,
		MaxBytes: request.Bytes, Digest: digest, UploadID: uploadID, ExpiresAt: expires, ContentType: MediaType,
	}); err != nil {
		if uploadID != nil {
			_ = target.Store.AbortMultipart(ctx, ref, storage.UploadID(*uploadID))
		}
		return UploadGrant{}, err
	}
	return answer, nil
}

// checkQuota refuses a segment that would take the project past its ceiling.
// The thread's own live segments are left out: a COMPACT supersedes them.
func (s *Service) checkQuota(ctx context.Context, thread Thread, bytes int64, limits Limits) error {
	project, err := s.repo.ProjectWorkspaceBytes(ctx, thread.ProjectID)
	if err != nil {
		return err
	}
	own, err := s.repo.ThreadWorkspaceBytes(ctx, thread)
	if err != nil {
		return err
	}
	if project-own+bytes > limits.MaxProjectWorkspaceBytes {
		return ErrQuotaExceeded
	}
	return nil
}

// PutObject is the streaming facade's write (backends without presign). The
// bytes go to a temporary key of the grant (`<segment key>.upload-<grant>`),
// measured and hashed as they arrive. Only bytes that match the declared size
// and digest are promoted to the content-addressed key, and only when that
// key does not already hold them. A mismatch deletes the temporary object
// only: the content-addressed key is never overwritten by, or deleted for, a
// bad upload. A retry after a lost acknowledgement therefore finds the
// segment in place, whatever the retry sends.
func (s *Service) PutObject(ctx context.Context, thread Thread, caller Caller, sha string, body io.Reader) error {
	if !validThread(thread) || !validCaller(caller) || !ValidSHA256(sha) || body == nil {
		return ErrInvalid
	}
	row, err := s.repo.ReadThread(ctx, thread)
	if err != nil {
		return err
	}
	if caller.Writer == WriterDesktop && row.OwnerUserID != caller.UserID {
		return ErrNotFound
	}
	if row.State != WriterState(caller.Writer) {
		return ErrStaleEpoch
	}
	key := ObjectKey(thread.ConversationUUID, sha)
	grant, ok, err := s.repo.OpenGrant(ctx, thread.ProjectID, key)
	if err != nil {
		return err
	}
	if !ok || grant.UploadID != nil || !grant.ExpiresAt.After(s.now()) {
		return ErrNotFound
	}
	final, err := s.objectRef(ctx, thread.ProjectID, key)
	if err != nil {
		return ErrInvalid
	}
	target := s.buckets.write()
	if s.holdsSegment(ctx, target.Store, final, grant.MaxBytes, sha) {
		// A retry after a lost acknowledgement: the segment is in place.
		return nil
	}
	temp, err := s.objectRef(ctx, thread.ProjectID, key+".upload-"+grant.ID)
	if err != nil {
		return ErrInvalid
	}
	hasher := sha256.New()
	limited := &countingReader{reader: io.LimitReader(body, grant.MaxBytes+1), hasher: hasher}
	if _, err := target.Store.Put(ctx, temp, limited, storage.PutOptions{ContentType: MediaType, ContentLength: -1}); err != nil {
		_ = target.Store.Delete(ctx, temp)
		return fmt.Errorf("workspace store: facade put: %w", err)
	}
	if limited.n > grant.MaxBytes {
		_ = target.Store.Delete(ctx, temp)
		return ErrSegmentTooLarge
	}
	if limited.n != grant.MaxBytes || hex.EncodeToString(hasher.Sum(nil)) != sha {
		_ = target.Store.Delete(ctx, temp)
		return ErrChecksumMismatch
	}
	defer func() { _ = target.Store.Delete(ctx, temp) }()
	if s.holdsSegment(ctx, target.Store, final, grant.MaxBytes, sha) {
		return nil
	}
	return s.promote(ctx, target.Store, temp, final, grant.MaxBytes, sha)
}

// holdsSegment reports whether ref already holds the segment: its size, and
// its digest when the backend reports one.
func (s *Service) holdsSegment(ctx context.Context, store storage.ObjectStore, ref storage.ObjectRef, size int64, sha string) bool {
	info, err := store.Stat(ctx, ref)
	if err != nil || info.Size != size {
		return false
	}
	return len(info.DigestSHA256) != sha256.Size || hex.EncodeToString(info.DigestSHA256) == sha
}

// promote copies the verified temporary object to the content-addressed key.
// The object store has no server-side copy in its interface, so the bytes
// are read back and written once more, hashed again on the way.
func (s *Service) promote(ctx context.Context, store storage.ObjectStore, temp, final storage.ObjectRef, size int64, sha string) error {
	reader, _, err := store.Get(ctx, temp, nil)
	if err != nil {
		return fmt.Errorf("workspace store: read upload: %w", err)
	}
	defer func() { _ = reader.Close() }()
	digest, _ := hex.DecodeString(sha)
	hasher := sha256.New()
	counted := &countingReader{reader: io.LimitReader(reader, size+1), hasher: hasher}
	if _, err := store.Put(ctx, final, counted, storage.PutOptions{
		ContentType: MediaType, ContentLength: -1, ChecksumSHA256: digest,
	}); err != nil {
		return fmt.Errorf("workspace store: promote upload: %w", err)
	}
	if counted.n != size || hex.EncodeToString(hasher.Sum(nil)) != sha {
		// The temporary object changed under us; the final key was written
		// with other bytes. Publish verifies the object and refuses it.
		return ErrChecksumMismatch
	}
	return nil
}

type countingReader struct {
	reader io.Reader
	hasher io.Writer
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.n += int64(n)
		_, _ = r.hasher.Write(p[:n])
	}
	return n, err
}

// CompletedPart is one uploaded part of a multipart grant.
type CompletedPart struct {
	Number int32
	ETag   string
}

// CompleteUpload finishes a multipart grant.
// uploadID must be the one the grant answered.
func (s *Service) CompleteUpload(ctx context.Context, projectID int64, caller Caller, grantID, uploadID string, parts []CompletedPart) error {
	if projectID <= 0 || !validCaller(caller) || !ValidUUID(grantID) || uploadID == "" || len(parts) == 0 || len(parts) > 10000 {
		return ErrInvalid
	}
	grant, ok, err := s.repo.ReadGrant(ctx, projectID, grantID)
	if err != nil {
		return err
	}
	if !ok || grant.UploadID == nil || *grant.UploadID != uploadID || grant.ConsumedAt != nil || !grant.ExpiresAt.After(s.now()) {
		return ErrNotFound
	}
	conversation, ok := conversationOfKey(grant.Key)
	if !ok {
		return ErrNotFound
	}
	thread := Thread{ProjectID: projectID, ConversationUUID: conversation}
	row, err := s.repo.ReadThread(ctx, thread)
	if err != nil {
		return err
	}
	if caller.Writer == WriterDesktop && row.OwnerUserID != caller.UserID {
		return ErrNotFound
	}
	if row.State != WriterState(caller.Writer) {
		return ErrStaleEpoch
	}
	ref, err := s.objectRef(ctx, projectID, grant.Key)
	if err != nil {
		return ErrInvalid
	}
	converted := make([]storage.Part, 0, len(parts))
	for index, part := range parts {
		if part.Number != int32(index+1) || part.ETag == "" {
			return ErrInvalid
		}
		converted = append(converted, storage.Part{Number: part.Number, ETag: part.ETag})
	}
	if _, err := s.buckets.write().Store.CompleteMultipart(ctx, ref, storage.UploadID(*grant.UploadID), converted); err != nil {
		return fmt.Errorf("workspace store: complete multipart: %w", err)
	}
	return nil
}

// conversationOfKey reads the thread out of a segment key.
func conversationOfKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "t/")
	if !ok {
		return "", false
	}
	conversation, _, ok := strings.Cut(rest, "/")
	return conversation, ok && ValidUUID(conversation)
}

// PublishRequest is one publish.
type PublishRequest struct {
	Thread        Thread
	Caller        Caller
	Epoch         int64
	Kind          string
	SHA256        string
	Bytes         int64
	Prerequisites []string
	Refs          []RefUpdate
	ObjectFormat  string
}

func validatePublish(request PublishRequest) error {
	if !validThread(request.Thread) || !validCaller(request.Caller) || request.Epoch <= 0 ||
		!ValidSHA256(request.SHA256) || request.Bytes <= 0 || len(request.Refs) == 0 || len(request.Refs) > 8 {
		return ErrInvalid
	}
	if request.ObjectFormat != ObjectFormatSHA1 {
		if request.ObjectFormat == "sha256" {
			return ErrUnsupportedObjectFormat
		}
		return ErrInvalid
	}
	// Phase 1: COMPACT segments only, which have no prerequisites.
	if request.Kind != KindCompact || len(request.Prerequisites) != 0 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, ref := range request.Refs {
		if !ValidRefName(ref.Name) || seen[ref.Name] || !ValidSHA1(ref.New) ||
			(ref.Old != nil && !ValidSHA1(*ref.Old)) {
			return ErrInvalid
		}
		seen[ref.Name] = true
	}
	return nil
}

// Publish records one uploaded segment and moves the thread's refs.
func (s *Service) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	started := s.now()
	if err := validatePublish(request); err != nil {
		s.refused(ctx, request, started, err)
		return PublishResult{}, err
	}
	result, err := s.publish(ctx, request)
	if err != nil {
		s.refused(ctx, request, started, err)
		return PublishResult{}, err
	}
	if !result.Replayed {
		s.audit.StoreEvent(ctx, AuditEvent{
			Name: "store.publish", Thread: request.Thread, UserID: request.Caller.UserID,
			Writer: request.Caller.Writer, Epoch: request.Epoch, Seq: result.Seq,
			SHA256: request.SHA256, Bytes: request.Bytes, Count: len(request.Refs),
			Started: started, Succeeded: true,
		})
	}
	return result, nil
}

func (s *Service) publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if _, err := s.requireWriter(ctx, request.Thread, request.Caller, request.Epoch); err != nil {
		return PublishResult{}, err
	}
	limits, err := s.limits.Limits(ctx, request.Thread.ProjectID)
	if err != nil {
		return PublishResult{}, fmt.Errorf("workspace store: limits: %w", err)
	}
	if request.Bytes > limits.MaxSegmentBytes {
		return PublishResult{}, ErrSegmentTooLarge
	}
	key := ObjectKey(request.Thread.ConversationUUID, request.SHA256)
	existing, replay, err := s.repo.LiveSegment(ctx, request.Thread, request.SHA256)
	if err != nil {
		return PublishResult{}, err
	}
	physical := s.buckets.write().Name
	if replay && existing.Epoch == request.Epoch && existing.Bytes == request.Bytes {
		// A retried publish: the object was checked when the segment was
		// recorded. The transaction decides whether the refs match.
		physical = existing.PhysicalBucket
	} else {
		if err := s.checkQuota(ctx, request.Thread, request.Bytes, limits); err != nil {
			return PublishResult{}, err
		}
		if err := s.verifyObject(ctx, request, key); err != nil {
			return PublishResult{}, err
		}
	}
	return s.repo.Publish(ctx, PublishRecord{
		Thread: request.Thread, Caller: request.Caller, Epoch: request.Epoch, Kind: request.Kind,
		SHA256: request.SHA256, Bytes: request.Bytes, Prerequisites: []string{},
		Refs: request.Refs, ObjectKey: key, PhysicalBucket: physical,
		MaxProjectBytes: limits.MaxProjectWorkspaceBytes,
	})
}

// verifyObject checks the stored object BEFORE the transaction opens: its size
// first (Stat), then its SHA-256: the backend's reported full-object checksum
// when it has one, else a hash over the bytes. Only durable data is
// acknowledged, and no storage call runs inside the transaction. An object
// whose bytes do not match its own address is deleted: no reader can use it.
func (s *Service) verifyObject(ctx context.Context, request PublishRequest, key string) error {
	ref, err := s.objectRef(ctx, request.Thread.ProjectID, key)
	if err != nil {
		return ErrInvalid
	}
	target := s.buckets.write()
	info, err := target.Store.Stat(ctx, ref)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrSegmentMissing
	}
	if err != nil {
		return fmt.Errorf("workspace store: stat segment: %w", err)
	}
	if info.Size != request.Bytes {
		s.integrityFailed(ctx, request, "size")
		return ErrChecksumMismatch
	}
	if len(info.DigestSHA256) == sha256.Size {
		// The backend verified the bytes against the checksum the grant
		// bound and reports the digest it kept: no need to read them.
		if hex.EncodeToString(info.DigestSHA256) != request.SHA256 {
			_ = target.Store.Delete(ctx, ref)
			s.integrityFailed(ctx, request, "checksum")
			return ErrChecksumMismatch
		}
		return nil
	}
	// No full-object checksum (a backend without one, a multipart upload,
	// the facade): hash the bytes.
	body, _, err := target.Store.Get(ctx, ref, nil)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrSegmentMissing
	}
	if err != nil {
		return fmt.Errorf("workspace store: read segment: %w", err)
	}
	defer func() { _ = body.Close() }()
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(body, request.Bytes+1))
	if err != nil {
		return fmt.Errorf("workspace store: hash segment: %w", err)
	}
	if n != request.Bytes || hex.EncodeToString(hasher.Sum(nil)) != request.SHA256 {
		_ = target.Store.Delete(ctx, ref)
		s.integrityFailed(ctx, request, "checksum")
		return ErrChecksumMismatch
	}
	return nil
}

func (s *Service) integrityFailed(ctx context.Context, request PublishRequest, reason string) {
	s.audit.StoreEvent(ctx, AuditEvent{
		Name: "store.integrity_failed", Thread: request.Thread, UserID: request.Caller.UserID,
		Writer: request.Caller.Writer, Epoch: request.Epoch, SHA256: request.SHA256,
		Bytes: request.Bytes, Reason: reason, Started: s.now(),
	})
}

func (s *Service) refused(ctx context.Context, request PublishRequest, started time.Time, err error) {
	s.audit.StoreEvent(ctx, AuditEvent{
		Name: "store.publish_refused", Thread: request.Thread, UserID: request.Caller.UserID,
		Writer: request.Caller.Writer, Epoch: request.Epoch, SHA256: request.SHA256,
		Bytes: request.Bytes, Reason: refusalReason(err), Started: started,
	})
}

func refusalReason(err error) string {
	for _, known := range []struct {
		err    error
		reason string
	}{
		{ErrInvalid, "invalid"}, {ErrNotFound, "not_found"}, {ErrStaleEpoch, "stale_epoch"},
		{ErrRefNotAllowed, "ref_not_allowed"}, {ErrRefConflict, "ref_conflict"},
		{ErrSegmentMissing, "segment_missing"}, {ErrChecksumMismatch, "checksum_mismatch"},
		{ErrUnsupportedObjectFormat, "unsupported_object_format"}, {ErrSegmentTooLarge, "segment_too_large"},
		{ErrQuotaExceeded, "workspace_quota_exceeded"},
	} {
		if errors.Is(err, known.err) {
			return known.reason
		}
	}
	return "internal"
}

// FetchSegment is one segment of a fetch plan, with a URL valid until ExpiresAt.
type FetchSegment struct {
	Seq           int64
	Kind          string
	SHA256        string
	Bytes         int64
	Prerequisites []string
	Refs          map[string]string
	URL           string
	ExpiresAt     time.Time
}

// FetchPlan is the minimal chain a reader needs from since_seq.
type FetchPlan struct {
	HeadSeq  int64
	Refs     map[string]string
	Segments []FetchSegment
}

// Plan answers the fetch plan. Phase 1: the newest COMPACT at or after
// sinceSeq, alone.
func (s *Service) Plan(ctx context.Context, thread Thread, caller Caller, sinceSeq int64) (FetchPlan, error) {
	if !validThread(thread) || !validCaller(caller) || sinceSeq < 0 {
		return FetchPlan{}, ErrInvalid
	}
	row, err := s.repo.ReadThread(ctx, thread)
	if err != nil {
		return FetchPlan{}, err
	}
	if caller.Writer == WriterDesktop && row.OwnerUserID != caller.UserID {
		return FetchPlan{}, ErrNotFound
	}
	if caller.Writer == WriterCloudExecutor &&
		(row.State != StateCloudRunning || row.RunningExecutionID != caller.ExecutionID) {
		return FetchPlan{}, ErrStaleEpoch
	}
	rows, err := s.repo.FetchRows(ctx, thread, sinceSeq)
	if err != nil {
		return FetchPlan{}, err
	}
	plan := FetchPlan{HeadSeq: rows.HeadSeq, Refs: rows.Refs, Segments: []FetchSegment{}}
	if plan.Refs == nil {
		plan.Refs = map[string]string{}
	}
	if rows.Newest == nil {
		return plan, nil
	}
	segment := *rows.Newest
	url, expires, err := s.readURL(ctx, thread, segment)
	if err != nil {
		return FetchPlan{}, err
	}
	prerequisites := segment.Prerequisites
	if prerequisites == nil {
		prerequisites = []string{}
	}
	plan.Segments = append(plan.Segments, FetchSegment{
		Seq: segment.Seq, Kind: segment.Kind, SHA256: segment.SHA256, Bytes: segment.Bytes,
		Prerequisites: prerequisites, Refs: segment.Refs, URL: url, ExpiresAt: expires,
	})
	return plan, nil
}

func (s *Service) readURL(ctx context.Context, thread Thread, segment SegmentRow) (string, time.Time, error) {
	expires := s.now().Add(GrantTTL).UTC()
	store, ok := s.buckets.open(segment.PhysicalBucket)
	if !ok {
		return "", time.Time{}, fmt.Errorf("workspace store: segment %d is in an unknown physical bucket", segment.Seq)
	}
	if store.Capabilities().Presign {
		ref, err := s.objectRef(ctx, thread.ProjectID, segment.ObjectKey)
		if err != nil {
			return "", time.Time{}, ErrInvalid
		}
		url, err := store.PresignGet(ctx, ref, GrantTTL)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("workspace store: presign get: %w", err)
		}
		return url, expires, nil
	}
	return ObjectRoutePrefix + strconv.FormatInt(thread.ProjectID, 10) + "/" + thread.ConversationUUID + "/" +
		segment.SHA256, expires, nil
}

// OpenObject is the streaming facade's read: one stored segment's bytes.
func (s *Service) OpenObject(ctx context.Context, thread Thread, caller Caller, sha string) (io.ReadCloser, int64, error) {
	if !validThread(thread) || !validCaller(caller) || !ValidSHA256(sha) {
		return nil, 0, ErrInvalid
	}
	row, err := s.repo.ReadThread(ctx, thread)
	if err != nil {
		return nil, 0, err
	}
	if caller.Writer == WriterDesktop && row.OwnerUserID != caller.UserID {
		return nil, 0, ErrNotFound
	}
	segment, ok, err := s.repo.StoredSegment(ctx, thread, sha)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrNotFound
	}
	store, ok := s.buckets.open(segment.PhysicalBucket)
	if !ok {
		return nil, 0, ErrNotFound
	}
	ref, err := s.objectRef(ctx, thread.ProjectID, segment.ObjectKey)
	if err != nil {
		return nil, 0, ErrInvalid
	}
	body, info, err := store.Get(ctx, ref, nil)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("workspace store: open segment: %w", err)
	}
	return body, info.Size, nil
}

// EraseThreadObjects deletes every object of a thread and answers whether a
// listing of the thread prefix then came back empty in every physical bucket.
// The rows are retired first, so no reader is offered an object that is about
// to go; a thread whose listing is not yet empty is erased again by the next
// sweep.
func (s *Service) EraseThreadObjects(ctx context.Context, thread Thread) (bool, error) {
	if !validThread(thread) {
		return false, ErrInvalid
	}
	if err := s.repo.RetireThreadSegments(ctx, thread); err != nil {
		return false, err
	}
	name, err := s.bucketName(ctx, thread.ProjectID)
	if err != nil {
		return false, err
	}
	empty := true
	for _, physical := range s.buckets.all() {
		done, err := deletePrefix(ctx, physical.Store, thread.ProjectID, name, ThreadPrefix(thread.ConversationUUID), func(storage.ObjectInfo) bool { return true })
		if err != nil {
			return false, err
		}
		empty = empty && done
	}
	return empty, nil
}

// maxPrefixPages bounds one prefix sweep; what is left is swept next time.
const maxPrefixPages = 10

// deletePrefix deletes the objects under prefix that keep() selects and
// answers whether a final listing found none left.
func deletePrefix(
	ctx context.Context, store storage.ObjectStore, projectID int64, bucketName, prefix string, keep func(storage.ObjectInfo) bool,
) (bool, error) {
	bucket, err := storage.NewBucketRef(strconv.FormatInt(projectID, 10), bucketName)
	if err != nil {
		return false, ErrInvalid
	}
	token := ""
	for page := 0; page < maxPrefixPages; page++ {
		listing, err := store.List(ctx, storage.ListQuery{Bucket: bucket, KeyPrefix: prefix, MaxKeys: 1000, ContinuationToken: token})
		if errors.Is(err, storage.ErrNotFound) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("workspace store: list %s: %w", prefix, err)
		}
		refs := []storage.ObjectRef{}
		for _, object := range listing.Objects {
			if !keep(object) {
				continue
			}
			ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucketName, object.Key)
			if err != nil {
				continue
			}
			refs = append(refs, ref)
		}
		if len(refs) > 0 {
			if _, err := store.DeleteBatch(ctx, refs); err != nil {
				return false, fmt.Errorf("workspace store: delete %s: %w", prefix, err)
			}
		}
		if !listing.IsTruncated || listing.NextContinuationToken == "" {
			break
		}
		token = listing.NextContinuationToken
	}
	// The erasure counts only after a fresh listing of the prefix is empty
	// (decision 12): the deletes above are not taken on trust.
	check, err := store.List(ctx, storage.ListQuery{Bucket: bucket, KeyPrefix: prefix, MaxKeys: 1})
	if errors.Is(err, storage.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("workspace store: relist %s: %w", prefix, err)
	}
	return len(check.Objects) == 0, nil
}

// SweepResult counts one sweep.
type SweepResult struct {
	SupersededDeleted int
	OrphansDeleted    int
}

// SweepObjects deletes superseded segments' objects past SupersededGrace and
// orphan objects (no live row) older than OrphanGrace, in bounded batches.
func (s *Service) SweepObjects(ctx context.Context, batch int) (SweepResult, error) {
	if batch <= 0 {
		batch = 100
	}
	started := s.now()
	result := SweepResult{}
	claimed, err := s.repo.ClaimSupersededObjects(ctx, started.Add(-SupersededGrace), batch)
	if err != nil {
		return result, err
	}
	failed := []int64{}
	for _, sweep := range claimed {
		if sweep.Shared {
			continue
		}
		store, ok := s.buckets.open(sweep.PhysicalBucket)
		if !ok {
			failed = append(failed, sweep.ID)
			continue
		}
		ref, err := s.objectRef(ctx, sweep.ProjectID, sweep.ObjectKey)
		if err != nil {
			continue
		}
		if err := store.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.logger.WarnContext(ctx, "workspace store: superseded segment delete failed", "segment_id", sweep.ID, "err", err)
			failed = append(failed, sweep.ID)
			continue
		}
		result.SupersededDeleted++
	}
	if len(failed) > 0 {
		if err := s.repo.ReleaseObjectSweep(ctx, failed); err != nil {
			return result, err
		}
	}
	orphans, err := s.sweepOrphans(ctx, started)
	result.OrphansDeleted = orphans
	if result.SupersededDeleted+result.OrphansDeleted > 0 {
		s.audit.StoreEvent(ctx, AuditEvent{
			Name: "store.gc", Count: result.SupersededDeleted + result.OrphansDeleted,
			Reason:  fmt.Sprintf("superseded=%d orphans=%d", result.SupersededDeleted, result.OrphansDeleted),
			Started: started, Succeeded: err == nil,
		})
	}
	return result, err
}

// maxOrphanProjects bounds the projects one sweep lists.
const maxOrphanProjects = 20

func (s *Service) sweepOrphans(ctx context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	after := s.orphanCursor
	s.mu.Unlock()
	projects, err := s.repo.WorkspaceProjects(ctx, after, maxOrphanProjects)
	if err != nil {
		return 0, err
	}
	// The cursor walks every project over successive sweeps, then wraps.
	next := int64(0)
	if len(projects) == maxOrphanProjects {
		next = projects[len(projects)-1]
	}
	s.mu.Lock()
	s.orphanCursor = next
	s.mu.Unlock()
	deleted := 0
	for _, projectID := range projects {
		name, err := s.bucketName(ctx, projectID)
		if err != nil {
			return deleted, err
		}
		for _, physical := range s.buckets.all() {
			var keepErr error
			_, err := deletePrefix(ctx, physical.Store, projectID, name, "t/", func(object storage.ObjectInfo) bool {
				if keepErr != nil || object.LastModified.IsZero() || now.Sub(object.LastModified) < OrphanGrace {
					return false
				}
				live, err := s.repo.KeyHasLiveObject(ctx, projectID, object.Key)
				if err != nil {
					keepErr = err
					return false
				}
				if !live {
					deleted++
				}
				return !live
			})
			if keepErr != nil {
				return deleted, keepErr
			}
			if err != nil {
				return deleted, err
			}
		}
	}
	return deleted, nil
}

func newGrantID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// ThreadBytes answers the size of a thread's live cloud copy (the banner's
// `workspace_bytes`).
func (s *Service) ThreadBytes(ctx context.Context, thread Thread) (int64, error) {
	if !validThread(thread) {
		return 0, ErrInvalid
	}
	return s.repo.ThreadWorkspaceBytes(ctx, thread)
}

// ProjectBytes answers the size of a project's live cloud copies.
func (s *Service) ProjectBytes(ctx context.Context, projectID int64) (int64, error) {
	if projectID <= 0 || projectID > 2147483647 {
		return 0, ErrInvalid
	}
	return s.repo.ProjectWorkspaceBytes(ctx, projectID)
}

// UploadSegment is the cloud executor's claim-bound upload (the later
// WorkspaceUpload gRPC call): no grant and no URL, the worker streams the
// bundle to Main, which stores it only when its bytes match the declared size
// and SHA-256. A digest the thread already stores answers exists=true.
func (s *Service) UploadSegment(
	ctx context.Context, thread Thread, caller Caller, epoch int64, sha string, size int64, body io.Reader,
) (bool, error) {
	if !validThread(thread) || !validCaller(caller) || epoch <= 0 || !ValidSHA256(sha) || size <= 0 || body == nil {
		return false, ErrInvalid
	}
	if _, err := s.requireWriter(ctx, thread, caller, epoch); err != nil {
		return false, err
	}
	limits, err := s.limits.Limits(ctx, thread.ProjectID)
	if err != nil {
		return false, fmt.Errorf("workspace store: limits: %w", err)
	}
	if size > limits.MaxSegmentBytes {
		return false, ErrSegmentTooLarge
	}
	if _, stored, err := s.repo.LiveSegment(ctx, thread, sha); err != nil {
		return false, err
	} else if stored {
		return true, nil
	}
	if err := s.checkQuota(ctx, thread, size, limits); err != nil {
		return false, err
	}
	if _, err := s.ensureBucket(ctx, thread.ProjectID); err != nil {
		return false, err
	}
	ref, err := s.objectRef(ctx, thread.ProjectID, ObjectKey(thread.ConversationUUID, sha))
	if err != nil {
		return false, ErrInvalid
	}
	target := s.buckets.write()
	hasher := sha256.New()
	counted := &countingReader{reader: io.LimitReader(body, size+1), hasher: hasher}
	if _, err := target.Store.Put(ctx, ref, counted, storage.PutOptions{ContentType: MediaType, ContentLength: -1}); err != nil {
		return false, fmt.Errorf("workspace store: executor put: %w", err)
	}
	if counted.n > size {
		_ = target.Store.Delete(ctx, ref)
		return false, ErrSegmentTooLarge
	}
	if counted.n != size || hex.EncodeToString(hasher.Sum(nil)) != sha {
		_ = target.Store.Delete(ctx, ref)
		return false, ErrChecksumMismatch
	}
	return false, nil
}
