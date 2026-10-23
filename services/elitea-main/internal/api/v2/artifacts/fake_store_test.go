package artifacts_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // ETag identity, not a security digest
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// fakeStore is a minimal in-memory storage.ObjectStore double. Only Put,
// List, and DeleteBatch see real use in this stage's tests (seeding fixtures
// and exercising DeleteBucket's cascade); the rest exist to satisfy the
// interface and return storage.ErrNotSupported.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string]storage.ObjectInfo
	data    map[string][]byte

	// firstPutReadAt records when Put's body argument was first read from —
	// S9's streaming-proof test (objects_test.go) uses this to confirm the
	// handler passes the multipart part straight through to Put rather than
	// buffering it first.
	firstPutReadAt time.Time

	// presign, when true, makes PresignGet/PresignPut succeed and
	// Capabilities report Presign — S15's grants_test.go sets this to
	// exercise CreateTransferGrant's real-presign branch; every other test
	// in this package relies on the false default (facade fallback /
	// ErrNotSupported), unchanged from before S15.
	presign bool

	// multipart, when true, makes StartMultipart/PresignPart/CompleteMultipart
	// succeed and Capabilities report NativeMultipart — S16's
	// multipart_test.go sets this to exercise CreateTransferGrant's
	// native-multipart branch; every other test in this package relies on
	// the false default, unchanged from before S16.
	multipart bool
	// nextUploadNum generates deterministic, unique upload ids —
	// Date.now()/math/rand are unnecessary here and a counter keeps test
	// output stable.
	nextUploadNum int
	// uploadContentType records the ContentType StartMultipart was called
	// with, keyed by upload id, so CompleteMultipart can report it back on
	// the assembled object the same way a real backend's own multipart
	// session would.
	uploadContentType map[storage.UploadID]string
	// uploadParts holds each part's bytes, keyed by upload id then part
	// number — simulateMultipartPartUpload (test helper, not part of
	// storage.ObjectStore) writes here directly, standing in for the
	// out-of-band presigned PUT a real client would perform against the URL
	// PresignPart returns; CompleteMultipart reads back from here.
	uploadParts map[storage.UploadID]map[int32][]byte
	// uploadPartETags records the ETag simulateMultipartPartUpload handed
	// back for each part, so CompleteMultipart can reject a request that
	// supplies the wrong one — modeling real S3/Azure CompleteMultipartUpload
	// behavior (an InvalidPart-style error on an ETag mismatch), which this
	// fake did not enforce at all before this check was added.
	uploadPartETags map[storage.UploadID]map[int32]string

	// beforeCompleteMultipart/beforeAbortMultipart, when set, run
	// synchronously as the very first thing inside CompleteMultipart/
	// AbortMultipart — before either method takes s.mu. This is the one
	// hook point multipart_test.go's concurrency regression tests need: it
	// lets a test synchronously make a *second*, "concurrent" HTTP request
	// land exactly inside the window a real race would occupy (after the
	// handler's own ownership/claim checks have already passed, but before
	// the backend call those checks were guarding), without needing real
	// goroutines or flaky timing. Deliberately called before the lock, not
	// after: fakeStore's own mutex is not reentrant, and the hook's nested
	// request will itself call back into these same methods.
	beforeCompleteMultipart func()
	beforeAbortMultipart    func()

	// failKeys names the object keys DeleteBatch must refuse. A real
	// backend reports such a key in BatchResult.Failed. It still deletes
	// the rest of the batch. S3 answers a per-key AccessDenied for an
	// object under an object-lock, for example. The fake deleted every
	// key unconditionally, so no test could reach DeleteBucket's
	// partial-failure path at all.
	failKeys map[string]bool
}

// firstReadRecorder wraps an io.Reader and calls onFirstRead exactly once,
// before the wrapped Reader's first Read returns.
type firstReadRecorder struct {
	r           io.Reader
	once        sync.Once
	onFirstRead func()
}

func (f *firstReadRecorder) Read(p []byte) (int, error) {
	f.once.Do(f.onFirstRead)
	return f.r.Read(p)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		objects:           make(map[string]storage.ObjectInfo),
		data:              make(map[string][]byte),
		uploadContentType: make(map[storage.UploadID]string),
		uploadParts:       make(map[storage.UploadID]map[int32][]byte),
		uploadPartETags:   make(map[storage.UploadID]map[int32]string),
	}
}

// simulateMultipartPartUpload records part data directly into the fake's
// tracked session, standing in for the out-of-band presigned PUT a real
// client would perform against PresignPart's returned URL — commit-side
// code only ever observes the assembled object through CompleteMultipart
// and Get, so it cannot tell the difference. Returns a synthetic ETag the
// test then reports back in CompleteMultipartUpload's request body, exactly
// as a real client would report the ETag its presigned PUT received.
func (s *fakeStore) simulateMultipartPartUpload(id storage.UploadID, part int32, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadParts[id] == nil {
		s.uploadParts[id] = make(map[int32][]byte)
	}
	if s.uploadPartETags[id] == nil {
		s.uploadPartETags[id] = make(map[int32]string)
	}
	s.uploadParts[id][part] = data
	etag := fmt.Sprintf("etag-%s-%d", id, part)
	s.uploadPartETags[id][part] = etag
	return etag
}

func (s *fakeStore) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

// seed adds an object of the given size whose bytes are all zero, directly,
// bypassing Put, for test fixture setup. Listing tests only ever read the
// size; use seedContent when the bytes themselves matter.
func (s *fakeStore) seed(projectID, bucket, key string, size int64) {
	s.seedContent(projectID, bucket, key, make([]byte, size), "")
}

// seedContent is seed with real bytes and a content type. Download tests need
// both: a fixture of zero bytes could not distinguish "served the object" from
// "served an empty body", and a fixture with no content type could not
// distinguish the stored type from the extension-derived fallback.
func (s *fakeStore) seedContent(projectID, bucket, key string, data []byte, contentType string) {
	ref, err := storage.NewObjectRef(projectID, bucket, key)
	if err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	s.objects[storageKey] = storage.ObjectInfo{
		Key: key, Size: int64(len(data)), LastModified: time.Now(), ContentType: contentType,
		ETag: contentETag(data),
	}
	s.data[storageKey] = data
}

// contentETag is the content-addressed ETag every real backend reports for a
// single-shot write (S3, Azure and the local disk backend all derive it from
// the bytes). This fake previously reported none at all, which left every
// ETag assertion vacuous: a handler that dropped the header entirely, or
// reported one object's ETag for another, looked identical to a correct one.
// Quoted, as HTTP requires for an opaque validator.
func contentETag(data []byte) string {
	return fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum(data))) //nolint:gosec // ETag identity, not a security digest
}

func (s *fakeStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	body = &firstReadRecorder{r: body, onFirstRead: func() {
		s.mu.Lock()
		s.firstPutReadAt = time.Now()
		s.mu.Unlock()
	}}
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	info := storage.ObjectInfo{
		Key: ref.Key(), Size: int64(len(data)), LastModified: time.Now(),
		ContentType: opts.ContentType, ETag: contentETag(data),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	s.objects[storageKey] = info
	s.data[storageKey] = data
	return info, nil
}

// Get honours rng, like every real backend. An earlier version ignored it
// and always returned the whole object with the whole object's Size, which
// made the handler's 206 path untestable: a missing Content-Range and a
// Content-Length that did not match the body both looked correct here.
func (s *fakeStore) Get(_ context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	info, ok := s.objects[storageKey]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	data := s.data[storageKey]
	info.TotalSize = int64(len(data))
	if rng != nil {
		start := rng.Start
		if start > int64(len(data)) {
			start = int64(len(data))
		}
		end := int64(len(data))
		if rng.End >= 0 && rng.End+1 < end {
			end = rng.End + 1
		}
		data = data[start:end]
		info.Size = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data)), info, nil
}

func (s *fakeStore) Stat(_ context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.objects[ref.StorageKey("")]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return info, nil
}

func (s *fakeStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	delete(s.objects, storageKey)
	delete(s.data, storageKey)
	return nil
}

func (s *fakeStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := storage.BatchResult{}
	for _, ref := range refs {
		if s.failKeys[ref.Key()] {
			result.Failed = append(result.Failed, storage.BatchError{
				Key: ref.Key(), Err: fmt.Errorf("fakeStore: delete refused for %q", ref.Key()),
			})
			continue
		}
		storageKey := ref.StorageKey("")
		delete(s.objects, storageKey)
		delete(s.data, storageKey)
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func (s *fakeStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	basePrefix := q.Bucket.BucketPrefix("")
	s.mu.Lock()
	defer s.mu.Unlock()

	var keys []string
	for storageKey := range s.objects {
		if !strings.HasPrefix(storageKey, basePrefix) {
			continue
		}
		logicalKey := strings.TrimPrefix(storageKey, basePrefix)
		if q.KeyPrefix != "" && !strings.HasPrefix(logicalKey, q.KeyPrefix) {
			continue
		}
		keys = append(keys, logicalKey)
	}
	sort.Strings(keys)

	page := storage.ListPage{Objects: make([]storage.ObjectInfo, 0, len(keys))}
	// Delimiter rollup, S3 semantics: a key whose remainder after KeyPrefix
	// still contains the delimiter is not listed as an object — it collapses
	// into a common prefix covering everything under that first delimiter.
	// An empty delimiter disables grouping entirely, which is exactly the
	// recursive listing the SDK asks for by omitting the parameter.
	//
	// This was previously unimplemented (delimiter was accepted and
	// ignored), which made every listing here implicitly recursive and left
	// CommonPrefixes permanently empty. No existing test passed a delimiter,
	// so honouring it changes no established expectation — but without it a
	// test of folder grouping would pass against a fake that cannot group,
	// proving nothing.
	seenPrefixes := map[string]bool{}
	for _, k := range keys {
		if q.Delimiter != "" {
			remainder := strings.TrimPrefix(k, q.KeyPrefix)
			if idx := strings.Index(remainder, q.Delimiter); idx >= 0 {
				group := q.KeyPrefix + remainder[:idx+len(q.Delimiter)]
				if !seenPrefixes[group] {
					seenPrefixes[group] = true
					page.CommonPrefixes = append(page.CommonPrefixes, group)
				}
				continue
			}
		}
		page.Objects = append(page.Objects, s.objects[basePrefix+k])
	}
	sort.Strings(page.CommonPrefixes)
	return page, nil
}

func (s *fakeStore) PresignGet(_ context.Context, ref storage.ObjectRef, _ time.Duration) (string, error) {
	if !s.presign {
		return "", storage.ErrNotSupported
	}
	return "https://presigned.example.test/get/" + ref.Key(), nil
}

func (s *fakeStore) PresignPut(_ context.Context, ref storage.ObjectRef, _ time.Duration, _ storage.PutOptions) (string, error) {
	if !s.presign {
		return "", storage.ErrNotSupported
	}
	return "https://presigned.example.test/put/" + ref.Key(), nil
}

func (s *fakeStore) StartMultipart(_ context.Context, _ storage.ObjectRef, opts storage.PutOptions) (storage.UploadID, error) {
	if !s.multipart {
		return "", storage.ErrNotSupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextUploadNum++
	id := storage.UploadID(fmt.Sprintf("upload-%d", s.nextUploadNum))
	s.uploadContentType[id] = opts.ContentType
	s.uploadParts[id] = make(map[int32][]byte)
	return id, nil
}

func (s *fakeStore) PresignPart(_ context.Context, ref storage.ObjectRef, id storage.UploadID, part int32, _ time.Duration) (string, error) {
	if !s.multipart {
		return "", storage.ErrNotSupported
	}
	return fmt.Sprintf("https://presigned.example.test/part/%s/%s/%d", ref.Key(), id, part), nil
}

// CompleteMultipart concatenates tracked parts in ascending part-number
// order — mirroring S3's own requirement that a multipart object's bytes
// are the concatenation of its parts in numeric order, regardless of the
// order parts were uploaded or listed in the request. Also validates each
// part's supplied ETag against the one simulateMultipartPartUpload issued —
// modeling real S3/Azure CompleteMultipartUpload's own InvalidPart-style
// rejection of a caller-supplied ETag that doesn't match what the backend
// actually has on record for that part, which this fake previously ignored
// entirely (the ETag field was accepted but never checked).
func (s *fakeStore) CompleteMultipart(_ context.Context, ref storage.ObjectRef, id storage.UploadID, parts []storage.Part) (storage.ObjectInfo, error) {
	if !s.multipart {
		return storage.ObjectInfo{}, storage.ErrNotSupported
	}
	if s.beforeCompleteMultipart != nil {
		s.beforeCompleteMultipart()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tracked, ok := s.uploadParts[id]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}

	sorted := make([]storage.Part, len(parts))
	copy(sorted, parts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })

	var assembled bytes.Buffer
	for _, p := range sorted {
		data, ok := tracked[p.Number]
		if !ok {
			return storage.ObjectInfo{}, fmt.Errorf("fakeStore: part %d was never uploaded for upload id %s", p.Number, id)
		}
		if want := s.uploadPartETags[id][p.Number]; p.ETag != want {
			return storage.ObjectInfo{}, fmt.Errorf("%w: part %d etag %q does not match %q", storage.ErrPreconditionFailed, p.Number, p.ETag, want)
		}
		assembled.Write(data)
	}

	info := storage.ObjectInfo{
		Key: ref.Key(), Size: int64(assembled.Len()), LastModified: time.Now(),
		ContentType: s.uploadContentType[id],
	}
	storageKey := ref.StorageKey("")
	s.objects[storageKey] = info
	s.data[storageKey] = assembled.Bytes()
	delete(s.uploadParts, id)
	delete(s.uploadPartETags, id)
	delete(s.uploadContentType, id)
	return info, nil
}

func (s *fakeStore) AbortMultipart(_ context.Context, _ storage.ObjectRef, id storage.UploadID) error {
	if !s.multipart {
		return storage.ErrNotSupported
	}
	if s.beforeAbortMultipart != nil {
		s.beforeAbortMultipart()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploadParts, id)
	delete(s.uploadPartETags, id)
	delete(s.uploadContentType, id)
	return nil
}

func (s *fakeStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{Presign: s.presign, NativeMultipart: s.multipart}
}

var _ storage.ObjectStore = (*fakeStore)(nil)
