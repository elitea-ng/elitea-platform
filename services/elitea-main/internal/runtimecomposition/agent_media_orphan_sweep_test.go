package runtimecomposition

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type fakeMediaReferences struct {
	projects   []int64
	referenced map[string]struct{}
	err        error
	// later, when set, is the set every read AFTER the first answers.
	later map[string]struct{}
	reads *int
}

func (f fakeMediaReferences) TenantProjects(context.Context) ([]int64, error) { return f.projects, nil }
func (f fakeMediaReferences) Referenced(context.Context) (map[string]struct{}, error) {
	if f.reads != nil {
		*f.reads++
		if *f.reads > 1 && f.later != nil {
			return f.later, f.err
		}
	}
	return f.referenced, f.err
}

// listingMediaStore lists one page of objects per project, two keys a page,
// and records deletes.
type listingMediaStore struct {
	storage.ObjectStore
	objects map[string][]storage.ObjectInfo // by project
	deleted []string
}

func (s *listingMediaStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	if q.Bucket.Bucket() != agentmediaapi.Bucket {
		return storage.ListPage{}, errors.New("wrong bucket")
	}
	objects, ok := s.objects[q.Bucket.ProjectID()]
	if !ok {
		return storage.ListPage{}, storage.ErrNotFound
	}
	start := 0
	if q.ContinuationToken != "" {
		start = int(q.ContinuationToken[0] - '0')
	}
	end := min(start+2, len(objects))
	page := storage.ListPage{Objects: objects[start:end]}
	if end < len(objects) {
		page.IsTruncated = true
		page.NextContinuationToken = string(rune('0' + end))
	}
	return page, nil
}

func (s *listingMediaStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	var result storage.BatchResult
	for _, ref := range refs {
		s.deleted = append(s.deleted, ref.ProjectID()+"/"+ref.Key())
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func mediaFile(c string) string { return strings.Repeat(c, 64) + ".png" }

func TestAgentMediaOrphanSweepDeletesOnlyOldUnnamedFiles(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 23, 0, 0, time.UTC)
	old := now.Add(-agentMediaOrphanGrace - time.Hour)
	fresh := now.Add(-time.Hour)
	store := &listingMediaStore{objects: map[string][]storage.ObjectInfo{
		"1": {
			{Key: mediaFile("a"), LastModified: old},   // named by a pack: kept
			{Key: mediaFile("b"), LastModified: old},   // unnamed and old: deleted
			{Key: mediaFile("c"), LastModified: fresh}, // unnamed but a recent draft: kept
			{Key: mediaFile("d")},                      // unknown age: kept
			{Key: mediaFile("e"), LastModified: old},   // second page, unnamed: deleted
		},
		"7": {
			{Key: mediaFile("f"), LastModified: old}, // named only by ANOTHER project: kept
		},
	}}
	references := fakeMediaReferences{
		projects: []int64{1, 7, 9}, // 9 has no bucket
		referenced: map[string]struct{}{
			repos.AgentMediaReferenceKey(1, mediaFile("a")): {},
			repos.AgentMediaReferenceKey(7, mediaFile("f")): {},
		},
	}
	sweep, err := newAgentMediaOrphanSweep(references, store)
	if err != nil {
		t.Fatal(err)
	}
	sweep.now = func() time.Time { return now }
	deleted, err := sweep.sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(store.deleted)
	want := []string{"1/" + mediaFile("b"), "1/" + mediaFile("e")}
	if deleted != 2 || strings.Join(store.deleted, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %d: %v, want %v", deleted, store.deleted, want)
	}
}

func TestAgentMediaOrphanSweepFailsClosedWithoutTheReferenceSet(t *testing.T) {
	store := &listingMediaStore{objects: map[string][]storage.ObjectInfo{
		"1": {{Key: mediaFile("b"), LastModified: time.Unix(0, 1)}},
	}}
	sweep, err := newAgentMediaOrphanSweep(fakeMediaReferences{projects: []int64{1}, err: errors.New("a tenant failed")}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sweep.sweep(context.Background()); err == nil {
		t.Fatal("a partial reference set was accepted")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("deleted %v without a reference set", store.deleted)
	}
}

func TestAgentMediaOrphanSweepRefusesAForeignOccurrence(t *testing.T) {
	sweep, err := newAgentMediaOrphanSweep(fakeMediaReferences{}, &listingMediaStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sweep.Execute(context.Background(), schedulingapp.Occurrence{JobID: "other"}); !errors.Is(err, errAgentMediaOrphanSweepInvalidOccurrence) {
		t.Fatalf("err = %v", err)
	}
	if _, err := newAgentMediaOrphanSweep(nil, nil); err == nil {
		t.Fatal("built without dependencies")
	}
}

// A file claimed after the tick read its reference snapshot is kept.
func TestAgentMediaOrphanSweepRechecksCandidatesAgainstAFreshRead(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 23, 0, 0, time.UTC)
	old := now.Add(-agentMediaOrphanGrace - time.Hour)
	store := &listingMediaStore{objects: map[string][]storage.ObjectInfo{
		"1": {{Key: mediaFile("a"), LastModified: old}, {Key: mediaFile("b"), LastModified: old}},
	}}
	reads := 0
	references := fakeMediaReferences{
		projects:   []int64{1},
		referenced: map[string]struct{}{},
		// A fork names "a" while the tick is running.
		later: map[string]struct{}{repos.AgentMediaReferenceKey(1, mediaFile("a")): {}},
		reads: &reads,
	}
	sweep, err := newAgentMediaOrphanSweep(references, store)
	if err != nil {
		t.Fatal(err)
	}
	sweep.now = func() time.Time { return now }
	if _, err := sweep.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(store.deleted, ",") != "1/"+mediaFile("b") {
		t.Fatalf("deleted %v, want only the file nobody claimed", store.deleted)
	}
}
