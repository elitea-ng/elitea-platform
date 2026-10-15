// Package workspacestoretest holds an in-memory storage.ObjectStore for the
// workspace store's tests. The BYTES are faked; what the tests prove lives in
// PostgreSQL and in the store's own checks.
package workspacestoretest

import (
	"bytes"
	"context"
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
	Deletes   int
	// FailDelete makes every Delete fail.
	FailDelete bool
}

// New answers an empty store; presign selects the backend capability.
func New(presign bool) *Store {
	return &Store{objects: map[string][]byte{}, modified: map[string]time.Time{}, Presign: presign}
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

func (s *Store) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
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
	return io.NopCloser(bytes.NewReader(content)), storage.ObjectInfo{
		Key: ref.Key(), Size: int64(len(content)), TotalSize: int64(len(content)), LastModified: s.modified[key(ref)],
	}, nil
}

func (s *Store) Stat(_ context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[key(ref)]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(content)), LastModified: s.modified[key(ref)]}, nil
}

func (s *Store) Delete(_ context.Context, ref storage.ObjectRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailDelete {
		return fmt.Errorf("delete refused")
	}
	delete(s.objects, key(ref))
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

func (s *Store) PresignPut(_ context.Context, ref storage.ObjectRef, _ time.Duration, _ storage.PutOptions) (string, error) {
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

func (s *Store) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error { return nil }

func (s *Store) Capabilities() storage.Capabilities {
	return storage.Capabilities{Presign: s.Presign, NativeMultipart: s.Multipart}
}

var _ storage.ObjectStore = (*Store)(nil)
