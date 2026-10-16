// Package workspacestoretest holds an in-memory storage.ObjectStore for the
// workspace store's tests. The BYTES are faked; what the tests prove lives in
// PostgreSQL and in the store's own checks.
package workspacestoretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// Store is a project-scoped in-memory object store.
type Store struct {
	mu        sync.Mutex
	objects   map[string][]byte
	modified  map[string]time.Time
	Presign   bool
	Multipart bool
	// Checksum is the backend's checksum capability: a PUT with a SHA-256
	// checksum is refused unless the bytes match, and Stat reports the digest.
	Checksum bool
	Deletes  int
	// Gets counts full reads (Get), so a test can prove a publish did not
	// stream the object.
	Gets int
	// Copier makes the store a server-side copier (Copy); Copies counts the
	// copies.
	Copier bool
	Copies int
	// BlockStat, when set, makes every Stat wait until it is closed (a
	// publish held in flight). StatEntered, when set, receives one value
	// each time a Stat starts waiting on it.
	BlockStat   chan struct{}
	StatEntered chan struct{}
	digests     map[string][]byte
	presigned   map[string][]byte
	// FailDelete makes every Delete fail.
	FailDelete bool
}

// New answers an empty store; presign selects the backend capability.
func New(presign bool) *Store {
	return &Store{
		objects: map[string][]byte{}, modified: map[string]time.Time{}, Presign: presign,
		digests: map[string][]byte{}, presigned: map[string][]byte{},
	}
}

func key(ref storage.ObjectRef) string {
	return ref.ProjectID() + "/" + ref.Bucket() + "/" + ref.Key()
}

// Seed stores bytes directly, as a client's presigned PUT would.
func (s *Store) Seed(ref storage.ObjectRef, content []byte, modified time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key(ref)] = append([]byte(nil), content...)
	s.modified[key(ref)] = modified
	delete(s.digests, key(ref))
}

// SeedWithChecksum stores bytes as a checksum-verified PUT would: the backend
// records the SHA-256 of the bytes it kept, and Stat reports it.
func (s *Store) SeedWithChecksum(ref storage.ObjectRef, content []byte) {
	digest := sha256.Sum256(content)
	s.SeedWithDigest(ref, content, digest[:])
}

// SeedWithDigest stores bytes with a reported digest that need not match
// them: a backend that reports a wrong checksum.
func (s *Store) SeedWithDigest(ref storage.ObjectRef, content, digest []byte) {
	s.Seed(ref, content, time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digests[key(ref)] = append([]byte(nil), digest...)
}

// PresignedChecksum answers the checksum a presigned PUT was bound to.
func (s *Store) PresignedChecksum(ref storage.ObjectRef) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.presigned[key(ref)]
}

// GetCount answers how many full reads ran.
func (s *Store) GetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Gets
}

// Has reports whether an object is stored.
func (s *Store) Has(ref storage.ObjectRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key(ref)]
	return ok
}

// Count answers how many objects are stored.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *Store) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if s.Checksum && len(opts.ChecksumSHA256) == sha256.Size {
		digest := sha256.Sum256(content)
		if !bytes.Equal(digest[:], opts.ChecksumSHA256) {
			return storage.ObjectInfo{}, fmt.Errorf("BadDigest")
		}
		s.SeedWithChecksum(ref, content)
		return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(content))}, nil
	}
	s.Seed(ref, content, time.Now())
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(content))}, nil
}

func (s *Store) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[key(ref)]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	s.Gets++
	return io.NopCloser(bytes.NewReader(content)), storage.ObjectInfo{
		Key: ref.Key(), Size: int64(len(content)), TotalSize: int64(len(content)), LastModified: s.modified[key(ref)],
	}, nil
}

func (s *Store) Stat(ctx context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	s.mu.Lock()
	block, entered := s.BlockStat, s.StatEntered
	s.mu.Unlock()
	if block != nil {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		select {
		case <-block:
		case <-ctx.Done():
			return storage.ObjectInfo{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[key(ref)]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	info := storage.ObjectInfo{Key: ref.Key(), Size: int64(len(content)), LastModified: s.modified[key(ref)]}
	if s.Checksum {
		info.DigestSHA256 = s.digests[key(ref)]
	}
	return info, nil
}

func (s *Store) Delete(_ context.Context, ref storage.ObjectRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailDelete {
		return fmt.Errorf("delete refused")
	}
	delete(s.objects, key(ref))
	delete(s.digests, key(ref))
	s.Deletes++
	return nil
}

func (s *Store) DeleteBatch(ctx context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	result := storage.BatchResult{}
	for _, ref := range refs {
		if err := s.Delete(ctx, ref); err != nil {
			result.Failed = append(result.Failed, storage.BatchError{Key: ref.Key(), Err: err})
			continue
		}
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func (s *Store) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := q.Bucket.ProjectID() + "/" + q.Bucket.Bucket() + "/" + q.KeyPrefix
	keys := []string{}
	for stored := range s.objects {
		if strings.HasPrefix(stored, prefix) {
			keys = append(keys, stored)
		}
	}
	sort.Strings(keys)
	page := storage.ListPage{}
	max := int(q.MaxKeys)
	if max <= 0 {
		max = 1000
	}
	for index, stored := range keys {
		if index == max {
			page.IsTruncated = true
			break
		}
		relative := strings.TrimPrefix(stored, q.Bucket.ProjectID()+"/"+q.Bucket.Bucket()+"/")
		page.Objects = append(page.Objects, storage.ObjectInfo{
			Key: relative, Size: int64(len(s.objects[stored])), LastModified: s.modified[stored],
		})
	}
	return page, nil
}

func (s *Store) PresignGet(_ context.Context, ref storage.ObjectRef, _ time.Duration) (string, error) {
	return "https://store.test/get/" + key(ref), nil
}

func (s *Store) PresignPut(_ context.Context, ref storage.ObjectRef, _ time.Duration, opts storage.PutOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presigned[key(ref)] = append([]byte(nil), opts.ChecksumSHA256...)
	return "https://store.test/put/" + key(ref), nil
}

func (s *Store) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "upload-1", nil
}

func (s *Store) PresignPart(_ context.Context, ref storage.ObjectRef, _ storage.UploadID, part int32, _ time.Duration) (string, error) {
	return fmt.Sprintf("https://store.test/part/%d/%s", part, key(ref)), nil
}

func (s *Store) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, nil
}

func (s *Store) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return nil
}

func (s *Store) Capabilities() storage.Capabilities {
	return storage.Capabilities{Presign: s.Presign, NativeMultipart: s.Multipart, ChecksumSHA256: s.Checksum}
}

// Copy copies inside the store when Copier is set, keeping the digest; it
// answers storage.ErrNotSupported otherwise, as a backend without a copy.
func (s *Store) Copy(_ context.Context, src, dst storage.ObjectRef) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Copier {
		return storage.ObjectInfo{}, storage.ErrNotSupported
	}
	content, ok := s.objects[key(src)]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	s.objects[key(dst)] = append([]byte(nil), content...)
	s.modified[key(dst)] = time.Now()
	if digest, ok := s.digests[key(src)]; ok {
		s.digests[key(dst)] = append([]byte(nil), digest...)
	} else {
		delete(s.digests, key(dst))
	}
	s.Copies++
	return storage.ObjectInfo{Key: dst.Key(), Size: int64(len(content))}, nil
}

// SetBlockStat sets BlockStat (and StatEntered) under the store's lock.
func (s *Store) SetBlockStat(block, entered chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BlockStat, s.StatEntered = block, entered
}

var (
	_ storage.ObjectStore  = (*Store)(nil)
	_ storage.ObjectCopier = (*Store)(nil)
)
