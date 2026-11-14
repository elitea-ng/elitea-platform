package agentmemoryvector

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

type fakeJournal struct {
	jobs      []Job
	entries   map[int64]Entry
	completed []Job
	failed    []Job
	class     string
	backoff   time.Duration
}

func (j *fakeJournal) Claim(context.Context, int, time.Duration) ([]Job, error) {
	jobs := j.jobs
	j.jobs = nil
	return jobs, nil
}

func (j *fakeJournal) Entries(_ context.Context, _ int64, ids []int64) (map[int64]Entry, error) {
	found := map[int64]Entry{}
	for _, id := range ids {
		if entry, ok := j.entries[id]; ok {
			found[id] = entry
		}
	}
	return found, nil
}

func (j *fakeJournal) Complete(_ context.Context, jobs []Job) error {
	j.completed = append(j.completed, jobs...)
	return nil
}

func (j *fakeJournal) Fail(_ context.Context, jobs []Job, class string, backoff time.Duration) error {
	j.failed = append(j.failed, jobs...)
	j.class, j.backoff = class, backoff
	return nil
}

type fakeEmbedder struct {
	err   error
	calls int
}

func (e *fakeEmbedder) Embed(_ context.Context, _ int64, inputs []string) (Embedding, error) {
	e.calls++
	if e.err != nil {
		return Embedding{}, e.err
	}
	vectors := make([][]float32, len(inputs))
	for i := range inputs {
		vectors[i] = []float32{1, float32(i), 0}
	}
	return Embedding{Model: "Text-Embedding 3/Small", Vectors: vectors}, nil
}

type fakeStore struct {
	upserts   map[int64][]Point // by agent
	space     Space
	token     string
	deleted   map[int64][]int64 // by agent
	hits      []Hit
	upsertErr error
	deleteErr error
	searchErr error
}

func (s *fakeStore) UpsertAgentMemory(_ context.Context, token string, _, applicationID int64, space Space, points []Point) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	if s.upserts == nil {
		s.upserts = map[int64][]Point{}
	}
	s.token, s.space = token, space
	s.upserts[applicationID] = append(s.upserts[applicationID], points...)
	return nil
}

func (s *fakeStore) SearchAgentMemory(context.Context, string, int64, int64, Space, []float32, uint32) ([]Hit, error) {
	return s.hits, s.searchErr
}

func (s *fakeStore) DeleteAgentMemoryEntries(_ context.Context, _, applicationID int64, ids []int64) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.deleted == nil {
		s.deleted = map[int64][]int64{}
	}
	s.deleted[applicationID] = append(s.deleted[applicationID], ids...)
	return nil
}

type fakeTokens struct{ minted int }

func (t *fakeTokens) Mint(projectID int64, source string, _ time.Duration) (string, time.Time, error) {
	t.minted++
	return "tok-" + source, time.Now().Add(time.Hour), nil
}

func newService(t *testing.T, journal *fakeJournal, embedder *fakeEmbedder, store *fakeStore) (*Service, *fakeTokens) {
	t.Helper()
	tokens := &fakeTokens{}
	service, err := New(journal, embedder, store, tokens, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, tokens
}

func TestRunOnceEmbedsActiveEntriesAndRemovesTheRest(t *testing.T) {
	journal := &fakeJournal{
		jobs: []Job{
			{ProjectID: 7, EntryID: 1, ApplicationID: 10, Generation: 1, Attempts: 1},
			{ProjectID: 7, EntryID: 2, ApplicationID: 10, Generation: 1, Attempts: 1},
			{ProjectID: 7, EntryID: 3, ApplicationID: 11, Generation: 4, Attempts: 1},
		},
		// Entry 2 was archived or deleted since the row was written: gone from
		// the active set.
		entries: map[int64]Entry{
			1: {ID: 1, ApplicationID: 10, Kind: "fact", Content: "one", Version: 2},
			3: {ID: 3, ApplicationID: 11, Kind: "episode", Content: "three", Version: 1},
		},
	}
	store := &fakeStore{}
	service, _ := newService(t, journal, &fakeEmbedder{}, store)
	closed, err := service.RunOnce(context.Background())
	if err != nil || closed != 3 {
		t.Fatalf("closed %d, err %v", closed, err)
	}
	if got := store.deleted[10]; len(got) != 1 || got[0] != 2 {
		t.Fatalf("removals: %v", store.deleted)
	}
	if len(store.upserts[10]) != 1 || store.upserts[10][0].EntryID != 1 || store.upserts[10][0].Version != 2 {
		t.Fatalf("agent 10 points: %+v", store.upserts[10])
	}
	if len(store.upserts[11]) != 1 || store.upserts[11][0].Text != "three" {
		t.Fatalf("agent 11 points: %+v", store.upserts[11])
	}
	if store.space.ModelSlug != "text-embedding-3-small" || store.space.Dimension != 3 || store.token != "tok-agent_memory" {
		t.Fatalf("space %+v token %q", store.space, store.token)
	}
	ids := []int64{}
	for _, job := range journal.completed {
		ids = append(ids, job.EntryID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) != 3 || len(journal.failed) != 0 {
		t.Fatalf("completed %v failed %v", ids, journal.failed)
	}
}

func TestAnEmbeddingOutageKeepsTheRowButNeverBlocksARemoval(t *testing.T) {
	journal := &fakeJournal{
		jobs: []Job{
			{ProjectID: 7, EntryID: 1, ApplicationID: 10, Generation: 1, Attempts: 3},
			{ProjectID: 7, EntryID: 2, ApplicationID: 10, Generation: 1, Attempts: 1},
		},
		entries: map[int64]Entry{1: {ID: 1, ApplicationID: 10, Kind: "fact", Content: "one", Version: 1}},
	}
	store := &fakeStore{}
	service, _ := newService(t, journal, &fakeEmbedder{err: errors.New("model down")}, store)
	closed, err := service.RunOnce(context.Background())
	if err == nil {
		t.Fatal("the outage was not reported")
	}
	// The removal of entry 2 needs no embedding: it went through.
	if closed != 1 || len(store.deleted[10]) != 1 || store.deleted[10][0] != 2 {
		t.Fatalf("closed %d, deleted %v", closed, store.deleted)
	}
	// The embedding of entry 1 is retried later, with its backoff.
	if len(journal.failed) != 1 || journal.failed[0].EntryID != 1 || journal.backoff != Backoff(3) {
		t.Fatalf("failed %v backoff %v", journal.failed, journal.backoff)
	}
	if len(store.upserts) != 0 {
		t.Fatalf("nothing should be written: %v", store.upserts)
	}
}

func TestAVectorOutageKeepsEveryRow(t *testing.T) {
	journal := &fakeJournal{
		jobs: []Job{
			{ProjectID: 7, EntryID: 1, ApplicationID: 10, Generation: 1, Attempts: 1},
			{ProjectID: 7, EntryID: 2, ApplicationID: 10, Generation: 1, Attempts: 1},
		},
		entries: map[int64]Entry{1: {ID: 1, ApplicationID: 10, Kind: "fact", Content: "one", Version: 1}},
	}
	store := &fakeStore{upsertErr: errors.New("down"), deleteErr: errors.New("down")}
	service, _ := newService(t, journal, &fakeEmbedder{}, store)
	closed, err := service.RunOnce(context.Background())
	if err == nil || closed != 0 || len(journal.completed) != 0 || len(journal.failed) != 2 {
		t.Fatalf("closed %d err %v completed %v failed %v", closed, err, journal.completed, journal.failed)
	}
}

func TestRankReturnsSimilarityByEntryAndFallsBackOnAnOutage(t *testing.T) {
	store := &fakeStore{hits: []Hit{{EntryID: 5, Score: 0.8}, {EntryID: 6, Score: 0.2}}}
	embedder := &fakeEmbedder{}
	service, tokens := newService(t, &fakeJournal{}, embedder, store)
	ranked, err := service.Rank(context.Background(), 7, 10, "  when is the deploy ")
	if err != nil || ranked[5] != 0.8 || ranked[6] != 0.2 || len(ranked) != 2 {
		t.Fatalf("ranked %v err %v", ranked, err)
	}
	if _, err := service.Rank(context.Background(), 7, 10, "again"); err != nil || tokens.minted != 1 {
		t.Fatalf("the token should be minted once and reused: %d %v", tokens.minted, err)
	}
	// A blank input ranks nothing and asks nothing.
	calls := embedder.calls
	if ranked, err := service.Rank(context.Background(), 7, 10, "   "); ranked != nil || err != nil || embedder.calls != calls {
		t.Fatalf("blank input: %v %v", ranked, err)
	}

	// An embedding outage is an error (recall uses the keyword ranking), and
	// the breaker then answers at once without another embedding call.
	embedder.err = errors.New("model down")
	if _, err := service.Rank(context.Background(), 7, 10, "x"); err == nil {
		t.Fatal("an outage returned a ranking")
	}
	calls = embedder.calls
	if _, err := service.Rank(context.Background(), 7, 10, "x"); !errors.Is(err, ErrUnavailable) || embedder.calls != calls {
		t.Fatalf("breaker: %v, calls %d -> %d", err, calls, embedder.calls)
	}
}

func TestAVectorSearchOutageIsAnError(t *testing.T) {
	store := &fakeStore{searchErr: errors.New("down")}
	service, _ := newService(t, &fakeJournal{}, &fakeEmbedder{}, store)
	if _, err := service.Rank(context.Background(), 7, 10, "x"); err == nil {
		t.Fatal("a search outage returned a ranking")
	}
}

func TestModelSlugFollowsTheVectorServiceRule(t *testing.T) {
	for model, want := range map[string]string{
		"text-embedding-3-small": "text-embedding-3-small",
		"Text Embedding/3 Small": "text-embedding-3-small",
		"--Edge--":               "edge",
		"qwen3:embedding-0.6b":   "qwen3-embedding-0-6b",
		"":                       "",
		"///":                    "",
		"a-very-long-model-name-0123456789-0123456789-0123456789-0123456789-abc": "a-very-long-model-name-0123456789-0123456789-0123456789-0123456",
	} {
		if got := ModelSlug(model); got != want {
			t.Errorf("ModelSlug(%q) = %q, want %q", model, got, want)
		}
		if got := ModelSlug(model); len(got) > 63 {
			t.Errorf("ModelSlug(%q) is %d long", model, len(got))
		}
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	if Backoff(1) != 15*time.Second || Backoff(2) != 30*time.Second || Backoff(100) != time.Hour {
		t.Fatalf("%v %v %v", Backoff(1), Backoff(2), Backoff(100))
	}
}

func TestNewRefusesAMissingDependency(t *testing.T) {
	if _, err := New(nil, &fakeEmbedder{}, &fakeStore{}, &fakeTokens{}, Config{}, nil); err == nil {
		t.Fatal("a nil journal was accepted")
	}
	if _, err := New(&fakeJournal{}, nil, &fakeStore{}, &fakeTokens{}, Config{}, nil); err == nil {
		t.Fatal("a nil embedder was accepted")
	}
}
