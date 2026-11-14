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
	models    map[int64]string
	requeued  []int64
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

func (j *fakeJournal) NoteModel(_ context.Context, projectID int64, slug string) (bool, error) {
	if j.models == nil {
		j.models = map[int64]string{}
	}
	previous, seen := j.models[projectID]
	j.models[projectID] = slug
	return seen && previous != slug, nil
}

func (j *fakeJournal) Requeue(_ context.Context, projectID int64) error {
	j.requeued = append(j.requeued, projectID)
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

func newService(t *testing.T, journal *fakeJournal, embedder Embedder, store *fakeStore) (*Service, *fakeTokens) {
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

// A fake embedder that refuses one text, as a model refuses an input it cannot
// take.
type pickyEmbedder struct{ refuse string }

func (e *pickyEmbedder) Embed(_ context.Context, _ int64, inputs []string) (Embedding, error) {
	vectors := make([][]float32, len(inputs))
	for i, input := range inputs {
		if input == e.refuse {
			return Embedding{}, errors.New("the gateway answered 400")
		}
		vectors[i] = []float32{1, 2, 3}
	}
	return Embedding{Model: "m", Vectors: vectors}, nil
}

func TestOneRefusedEntryDoesNotHoldBackItsNeighbours(t *testing.T) {
	journal := &fakeJournal{
		jobs: []Job{
			{ProjectID: 7, EntryID: 1, ApplicationID: 10, Generation: 1, Attempts: 1},
			{ProjectID: 7, EntryID: 2, ApplicationID: 10, Generation: 1, Attempts: 1},
			{ProjectID: 7, EntryID: 3, ApplicationID: 10, Generation: 1, Attempts: 1},
		},
		entries: map[int64]Entry{
			1: {ID: 1, ApplicationID: 10, Kind: "fact", Content: "good one", Version: 1},
			2: {ID: 2, ApplicationID: 10, Kind: "fact", Content: "poison", Version: 1},
			3: {ID: 3, ApplicationID: 10, Kind: "fact", Content: "good two", Version: 1},
		},
	}
	store := &fakeStore{}
	service, _ := newService(t, journal, &pickyEmbedder{refuse: "poison"}, store)
	closed, err := service.RunOnce(context.Background())
	if err == nil || closed != 2 {
		t.Fatalf("closed %d err %v", closed, err)
	}
	if len(store.upserts[10]) != 2 {
		t.Fatalf("the neighbours were not written: %+v", store.upserts[10])
	}
	if len(journal.failed) != 1 || journal.failed[0].EntryID != 2 {
		t.Fatalf("only the refused entry should wait: %v", journal.failed)
	}
}

func TestAProjectScopedFailureNeverOpensTheBreakerAndParksItsRows(t *testing.T) {
	embedder := &fakeEmbedder{err: ErrNoEmbeddingModel}
	journal := &fakeJournal{
		jobs:    []Job{{ProjectID: 7, EntryID: 1, ApplicationID: 10, Generation: 1, Attempts: 1}},
		entries: map[int64]Entry{1: {ID: 1, ApplicationID: 10, Kind: "fact", Content: "one", Version: 1}},
	}
	service, _ := newService(t, journal, embedder, &fakeStore{})
	// Project 7 has no model: its own recall fails, but another project's recall
	// is not switched off for it.
	if _, err := service.Rank(context.Background(), 7, 10, "x"); !errors.Is(err, ErrNoEmbeddingModel) {
		t.Fatalf("rank: %v", err)
	}
	embedder.err = nil
	if _, err := service.Rank(context.Background(), 8, 10, "x"); err != nil {
		t.Fatalf("another project's recall was switched off: %v", err)
	}
	// Its journal rows wait out the longest backoff at once.
	embedder.err = ErrNoEmbeddingModel
	if _, err := service.RunOnce(context.Background()); err == nil {
		t.Fatal("the failure was not reported")
	}
	if journal.backoff != time.Hour {
		t.Fatalf("backoff %v", journal.backoff)
	}
}

func TestACallersEndedContextNeverOpensTheBreaker(t *testing.T) {
	embedder := &fakeEmbedder{err: context.DeadlineExceeded}
	service, _ := newService(t, &fakeJournal{}, embedder, &fakeStore{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Rank(ctx, 7, 10, "x"); err == nil {
		t.Fatal("a cancelled caller got a ranking")
	}
	embedder.err = nil
	if _, err := service.Rank(context.Background(), 7, 10, "x"); err != nil {
		t.Fatalf("the breaker opened for a cancelled caller: %v", err)
	}
}

// A model whose name the test changes between calls.
type switchingEmbedder struct{ model string }

func (e *switchingEmbedder) Embed(_ context.Context, _ int64, inputs []string) (Embedding, error) {
	vectors := make([][]float32, len(inputs))
	for i := range inputs {
		vectors[i] = []float32{1, 2, 3}
	}
	return Embedding{Model: e.model, Vectors: vectors}, nil
}

func TestAChangeOfTheDefaultModelQueuesTheProjectsEntriesAgain(t *testing.T) {
	journal := &fakeJournal{}
	embedder := &switchingEmbedder{model: "model-a"}
	service, _ := newService(t, journal, embedder, &fakeStore{})
	ctx := context.Background()
	// The first use only records the model.
	if _, err := service.Rank(ctx, 7, 10, "x"); err != nil || len(journal.requeued) != 0 {
		t.Fatalf("first use: %v %v", err, journal.requeued)
	}
	if _, err := service.Rank(ctx, 7, 10, "y"); err != nil || len(journal.requeued) != 0 {
		t.Fatalf("same model: %v %v", err, journal.requeued)
	}
	// The default model changes: the next use queues the project once.
	embedder.model = "model-b"
	if _, err := service.Rank(ctx, 7, 10, "x"); err != nil || len(journal.requeued) != 1 || journal.requeued[0] != 7 {
		t.Fatalf("changed model: %v %v", err, journal.requeued)
	}
	if _, err := service.Rank(ctx, 7, 10, "x"); err != nil || len(journal.requeued) != 1 {
		t.Fatalf("a second use must not queue again: %v %v", err, journal.requeued)
	}
	// Another project is its own record.
	if _, err := service.Rank(ctx, 8, 10, "x"); err != nil || len(journal.requeued) != 1 {
		t.Fatalf("another project: %v %v", err, journal.requeued)
	}
}
