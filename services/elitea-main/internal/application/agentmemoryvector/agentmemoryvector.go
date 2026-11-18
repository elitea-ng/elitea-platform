// Package agentmemoryvector keeps a copy of an agent's active memory entries in
// elitea-vector (source agent_memory) so recall can rank by meaning
// (ADR-0050 decision 1b, issue 1657).
//
// THE ROWS STAY THE AUTHORITY. The vector is a derived copy, never the source
// of what is recalled:
//
//   - A trigger on every tenant's agent_memory_entries writes a journal row
//     (centry.agent_memory_vector_journal, shared/0248) in the transaction of
//     each change, so every path that changes an entry (the agent's tools, the
//     Memory routes, the entry limit, clear, opt-out, the retention purge) is
//     covered.
//   - Service.RunOnce works the journal. It reads each entry as it is NOW:
//     active and not a cursor, it embeds the content and writes the point;
//     anything else removes the point. Removal is the administrator's Delete,
//     so it needs no token and no embedding model: a model outage never keeps a
//     deleted or archived entry in the index.
//   - A failure (the model, the gateway, elitea-vector) leaves the row, with a
//     backoff, and never touches the memory write that caused it. The
//     reconciler is the only writer, so an old row never resurrects a vector.
//
// RECALL. Service.Rank embeds the turn's input and searches the agent's
// namespace. agentmemory.SelectRanked orders the platform's own candidate rows
// by that similarity, so the presence filter, the cursor rule and the budgets
// still come from the rows; the index never adds an entry. When the embedding
// or the search fails, Rank returns an error and recall falls back to the
// keyword ranking, never to no recall. A short breaker stops a dead dependency
// from adding its timeout to every turn.
package agentmemoryvector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Source is the keyword of the vector source and of the platform token's
// source.
const Source = "agent_memory"

// Job is one claimed journal row.
type Job struct {
	ProjectID     int64
	EntryID       int64
	ApplicationID int64
	// Generation is the row's obligation counter at the claim; Complete and
	// Fail act only on this generation.
	Generation int64
	// Attempts counts the runs so far, this one included.
	Attempts int
}

// Entry is an entry as the index must hold it: an active entry that is not a
// cursor.
type Entry struct {
	ID            int64
	ApplicationID int64
	Kind          string
	Content       string
	Version       int
}

// Space is an embedding space: the model slug and the vector length.
type Space struct {
	ModelSlug string
	Dimension uint32
}

// Point is one entry to write.
type Point struct {
	EntryID int64
	Version int
	Kind    string
	Text    string
	Vector  []float32
}

// Hit is one ranked entry.
type Hit struct {
	EntryID int64
	Score   float32
}

// Embedding is the answer of an embedding call: the model it used and one
// vector per input, in order.
type Embedding struct {
	Model   string
	Vectors [][]float32
}

// Journal is the work list (*repos.AgentMemoryVectorJournal).
type Journal interface {
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Job, error)
	Entries(ctx context.Context, projectID int64, ids []int64) (map[int64]Entry, error)
	Complete(ctx context.Context, jobs []Job) error
	Fail(ctx context.Context, jobs []Job, class string, backoff time.Duration) error
	// NoteModel records the model slug a project embeds with and answers
	// whether it replaced a different one (the default model changed).
	NoteModel(ctx context.Context, projectID int64, modelSlug string) (changed bool, err error)
	// Requeue enqueues every active entry of the project again.
	Requeue(ctx context.Context, projectID int64) error
}

// Embedder embeds texts for a project through the LLM gateway.
type Embedder interface {
	Embed(ctx context.Context, projectID int64, inputs []string) (Embedding, error)
}

// Store is elitea-vector (*vectorclient.Client).
type Store interface {
	UpsertAgentMemory(ctx context.Context, token string, projectID, applicationID int64, space Space, points []Point) error
	SearchAgentMemory(ctx context.Context, token string, projectID, applicationID int64, space Space, vector []float32, limit uint32) ([]Hit, error)
	DeleteAgentMemoryEntries(ctx context.Context, projectID, applicationID int64, entryIDs []int64) error
}

// Tokens mints the platform token (vectorintrospection.PlatformTokens).
type Tokens interface {
	Mint(projectID int64, source string, lifetime time.Duration) (string, time.Time, error)
}

// ErrProjectScoped marks a failure that belongs to one project, not to the
// dependency: the project has no embedding model, or the gateway refused its
// request (a budget, a policy, a model it may not use). It never opens the
// recall breaker, which would switch every other project off, and a journal
// row that fails with it waits out the longest backoff at once.
var ErrProjectScoped = errors.New("the failure belongs to this project")

// ErrNoEmbeddingModel reports a project with no embedding model to call.
var ErrNoEmbeddingModel = fmt.Errorf("agent memory: the project has no embedding model: %w", ErrProjectScoped)

// ErrUnavailable is Rank's answer while a dependency is down (the breaker is
// open): recall uses the keyword ranking.
var ErrUnavailable = errors.New("agent memory vectors are unavailable")

// Tuning. The values are the defaults of Config.
const (
	// DefaultBatch is how many journal rows one run claims.
	DefaultBatch = 64
	// DefaultLease is how long a claim holds its rows.
	DefaultLease = 3 * time.Minute
	// DefaultBusyPause and DefaultIdlePause pace Run: soon again after work,
	// a longer wait when the journal was empty.
	DefaultBusyPause = 500 * time.Millisecond
	DefaultIdlePause = 10 * time.Second
	// DefaultRankTimeout bounds one recall's embedding and search together.
	DefaultRankTimeout = 2500 * time.Millisecond
	// DefaultBreakerPause is how long Rank stays off after a failure.
	DefaultBreakerPause = 20 * time.Second
	// RankLimit is how many entries a search returns: the recall pool.
	RankLimit = 200
	// embedBatch is how many texts one embedding call carries.
	embedBatch = 32
	// tokenLifetime and tokenReuse: a token is minted for an hour and reused
	// until five minutes before it expires.
	tokenLifetime = time.Hour
	tokenReuse    = 5 * time.Minute

	maxBackoff  = time.Hour
	baseBackoff = 15 * time.Second
)

// Config tunes a Service; a zero field takes its default.
type Config struct {
	Batch        int
	Lease        time.Duration
	BusyPause    time.Duration
	IdlePause    time.Duration
	RankTimeout  time.Duration
	BreakerPause time.Duration
}

func (c Config) withDefaults() Config {
	if c.Batch <= 0 {
		c.Batch = DefaultBatch
	}
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	if c.BusyPause <= 0 {
		c.BusyPause = DefaultBusyPause
	}
	if c.IdlePause <= 0 {
		c.IdlePause = DefaultIdlePause
	}
	if c.RankTimeout <= 0 {
		c.RankTimeout = DefaultRankTimeout
	}
	if c.BreakerPause <= 0 {
		c.BreakerPause = DefaultBreakerPause
	}
	return c
}

// Service embeds and ranks. Build it with New.
type Service struct {
	journal  Journal
	embedder Embedder
	store    Store
	tokens   Tokens
	config   Config
	logger   *slog.Logger
	now      func() time.Time

	mutex      sync.Mutex
	cached     map[int64]cachedToken
	noted      map[int64]string
	offUntil   time.Time
	lastReport time.Time
}

type cachedToken struct {
	value   string
	expires time.Time
}

// New builds the service. Every dependency is required: a service that could
// not write, remove or search would be the half-wired state nobody notices.
func New(journal Journal, embedder Embedder, store Store, tokens Tokens, config Config, logger *slog.Logger) (*Service, error) {
	if journal == nil || embedder == nil || store == nil || tokens == nil {
		return nil, errors.New("agent memory vectors need a journal, an embedder, a vector store and a token source")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		journal: journal, embedder: embedder, store: store, tokens: tokens,
		config: config.withDefaults(), logger: logger, now: time.Now,
		cached: map[int64]cachedToken{}, noted: map[int64]string{},
	}, nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// ModelSlug is the embedding-space slug of a model name: lower case, every
// run of other characters one hyphen, at most 63 characters, never starting
// with a hyphen (elitea-vector's rule). It is empty for a name with nothing to
// keep.
func ModelSlug(model string) string {
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(model), "-"), "-")
	if len(slug) > 63 {
		slug = strings.TrimRight(slug[:63], "-")
	}
	return slug
}

func (s *Service) spaceOf(embedding Embedding) (Space, error) {
	slug := ModelSlug(embedding.Model)
	if slug == "" || len(embedding.Vectors) == 0 || len(embedding.Vectors[0]) == 0 {
		return Space{}, errors.New("the embedding names no model or holds no vector")
	}
	dimension := len(embedding.Vectors[0])
	for _, vector := range embedding.Vectors {
		if len(vector) != dimension {
			return Space{}, errors.New("the embedding's vectors differ in length")
		}
	}
	return Space{ModelSlug: slug, Dimension: uint32(dimension)}, nil //nolint:gosec // a vector length, bounded by the model
}

// noteModel watches the project's embedding model. The first write only records
// it; a different slug than the recorded one (an administrator changed the
// default model) enqueues every active entry of the project, so the entries
// move to the new space instead of ranking after the newer ones for ever. A
// failure is logged and retried on the next call (the in-process memo is set
// only after the journal took the note).
func (s *Service) noteModel(ctx context.Context, projectID int64, slug string) {
	s.mutex.Lock()
	known := s.noted[projectID] == slug
	s.mutex.Unlock()
	if known {
		return
	}
	changed, err := s.journal.NoteModel(ctx, projectID, slug)
	if err == nil && changed {
		err = s.journal.Requeue(ctx, projectID)
	}
	if err != nil {
		s.logger.Warn("agent memory: the embedding model of a project could not be noted", "project_id", projectID, "error", err)
		return
	}
	if changed {
		s.logger.Info("agent memory: the default embedding model changed; every active entry of the project is queued again",
			"project_id", projectID, "model", slug)
	}
	s.mutex.Lock()
	if len(s.noted) > 4096 {
		clear(s.noted)
	}
	s.noted[projectID] = slug
	s.mutex.Unlock()
}

// token returns a platform token for the project, minted once and reused.
func (s *Service) token(projectID int64) (string, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if cached, ok := s.cached[projectID]; ok && s.now().Add(tokenReuse).Before(cached.expires) {
		return cached.value, nil
	}
	value, expires, err := s.tokens.Mint(projectID, Source, tokenLifetime)
	if err != nil {
		return "", err
	}
	if len(s.cached) > 4096 {
		clear(s.cached)
	}
	s.cached[projectID] = cachedToken{value: value, expires: expires}
	return value, nil
}

// Rank answers each entry the index ranked for the query, by entry id with its
// cosine similarity. An error (ErrUnavailable included) means recall should
// use the keyword ranking; it is never worth failing a turn for.
func (s *Service) Rank(ctx context.Context, projectID, applicationID int64, query string) (map[int64]float32, error) {
	query = strings.TrimSpace(query)
	if query == "" || projectID <= 0 || applicationID <= 0 {
		return nil, nil
	}
	if s.breakerOpen() {
		return nil, ErrUnavailable
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, s.config.RankTimeout)
	defer cancel()
	ranked, err := s.rank(ctx, projectID, applicationID, query)
	if err != nil {
		// The breaker is for a dependency that is down or slow for everyone:
		// not for this caller's own ended context (a short deadline on the
		// turn, a cancel), and not for a failure of this project alone.
		if parent.Err() == nil && !errors.Is(err, ErrProjectScoped) {
			s.openBreaker(err)
		}
		return nil, err
	}
	return ranked, nil
}

func (s *Service) rank(ctx context.Context, projectID, applicationID int64, query string) (map[int64]float32, error) {
	// A query is cut where an entry is: one embedding is one bounded input.
	embedding, err := s.embedder.Embed(ctx, projectID, []string{truncate(query, 2048)})
	if err != nil {
		return nil, fmt.Errorf("embed the turn's input: %w", err)
	}
	space, err := s.spaceOf(embedding)
	if err != nil || len(embedding.Vectors) != 1 {
		return nil, errors.New("the embedding of the turn's input is unusable")
	}
	s.noteModel(ctx, projectID, space.ModelSlug)
	token, err := s.token(projectID)
	if err != nil {
		return nil, fmt.Errorf("mint the vector token: %w", err)
	}
	hits, err := s.store.SearchAgentMemory(ctx, token, projectID, applicationID, space, embedding.Vectors[0], RankLimit)
	if err != nil {
		return nil, fmt.Errorf("search the agent's memory vectors: %w", err)
	}
	ranked := make(map[int64]float32, len(hits))
	for _, hit := range hits {
		ranked[hit.EntryID] = hit.Score
	}
	return ranked, nil
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut]
}

func (s *Service) breakerOpen() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.now().Before(s.offUntil)
}

func (s *Service) openBreaker(cause error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.offUntil = s.now().Add(s.config.BreakerPause)
	// One line per pause at most: a dead dependency would otherwise log on
	// every turn.
	if s.now().Sub(s.lastReport) >= s.config.BreakerPause {
		s.lastReport = s.now()
		s.logger.Warn("agent memory: vector recall is off for a while; turns recall by keyword", "error", cause)
	}
}

// Run works the journal until ctx ends: soon again after work, a pause when
// there was none. It never returns an error: a failed pass is logged and the
// rows keep their backoff.
func (s *Service) Run(ctx context.Context) {
	// The first pass waits one idle pause: the composition starts this before
	// the rest of the boot is done, and a pass before the migrations ran would
	// only log an error.
	select {
	case <-ctx.Done():
		return
	case <-time.After(s.config.IdlePause):
	}
	for {
		worked, err := s.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("agent memory: a vector journal pass failed", "error", err)
		}
		pause := s.config.IdlePause
		if worked > 0 && err == nil {
			pause = s.config.BusyPause
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// RunOnce claims a batch of journal rows and works them. It answers how many
// rows it closed.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	jobs, err := s.journal.Claim(ctx, s.config.Batch, s.config.Lease)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	byProject := map[int64][]Job{}
	for _, job := range jobs {
		byProject[job.ProjectID] = append(byProject[job.ProjectID], job)
	}
	closed := 0
	var errs []error
	for projectID, projectJobs := range byProject {
		done, err := s.workProject(ctx, projectID, projectJobs)
		closed += done
		if err != nil {
			errs = append(errs, err)
		}
	}
	return closed, errors.Join(errs...)
}

// workProject works one project's claimed rows: removals first (they need
// only elitea-vector), then the embeddings.
func (s *Service) workProject(ctx context.Context, projectID int64, jobs []Job) (int, error) {
	ids := make([]int64, len(jobs))
	for i, job := range jobs {
		ids[i] = job.EntryID
	}
	entries, err := s.journal.Entries(ctx, projectID, ids)
	if err != nil {
		s.fail(ctx, jobs, "entries unavailable; retry", err)
		return 0, err
	}
	removalsByAgent := map[int64][]Job{}
	var upserts []Job
	for _, job := range jobs {
		if _, active := entries[job.EntryID]; active {
			upserts = append(upserts, job)
			continue
		}
		removalsByAgent[job.ApplicationID] = append(removalsByAgent[job.ApplicationID], job)
	}
	closed := 0
	var errs []error
	for applicationID, agentJobs := range removalsByAgent {
		entryIDs := make([]int64, len(agentJobs))
		for i, job := range agentJobs {
			entryIDs[i] = job.EntryID
		}
		if err := s.store.DeleteAgentMemoryEntries(ctx, projectID, applicationID, entryIDs); err != nil {
			s.fail(ctx, agentJobs, "vector removal failed; retry", err)
			errs = append(errs, fmt.Errorf("remove the vectors of agent %d (project %d): %w", applicationID, projectID, err))
			continue
		}
		if err := s.journal.Complete(ctx, agentJobs); err != nil {
			errs = append(errs, err)
			continue
		}
		closed += len(agentJobs)
	}
	done, err := s.embedAndWrite(ctx, projectID, upserts, entries)
	closed += done
	if err != nil {
		errs = append(errs, err)
	}
	return closed, errors.Join(errs...)
}

// embedAndWrite embeds the entries of upserts in batches and writes each
// agent's points.
func (s *Service) embedAndWrite(ctx context.Context, projectID int64, upserts []Job, entries map[int64]Entry) (int, error) {
	closed := 0
	var errs []error
	for start := 0; start < len(upserts); start += embedBatch {
		batch := upserts[start:min(start+embedBatch, len(upserts))]
		texts := make([]string, len(batch))
		for i, job := range batch {
			texts[i] = entries[job.EntryID].Content
		}
		embedding, err := s.embedder.Embed(ctx, projectID, texts)
		if (err != nil || len(embedding.Vectors) != len(batch)) && len(batch) > 1 && !errors.Is(err, ErrProjectScoped) {
			// One entry the model refuses (a gateway 400) must not hold back
			// its neighbours: work the batch entry by entry.
			for _, job := range batch {
				done, oneErr := s.embedAndWrite(ctx, projectID, []Job{job}, entries)
				closed += done
				if oneErr != nil {
					errs = append(errs, oneErr)
				}
			}
			continue
		}
		if err != nil || len(embedding.Vectors) != len(batch) {
			s.fail(ctx, batch, "embedding unavailable; retry", err)
			errs = append(errs, fmt.Errorf("embed %d entries (project %d): %w", len(batch), projectID, embeddingError(err)))
			continue
		}
		space, err := s.spaceOf(embedding)
		if err != nil {
			s.fail(ctx, batch, "embedding unusable; retry", err)
			errs = append(errs, fmt.Errorf("embed entries (project %d): %w", projectID, err))
			continue
		}
		s.noteModel(ctx, projectID, space.ModelSlug)
		token, err := s.token(projectID)
		if err != nil {
			s.fail(ctx, batch, "vector token unavailable; retry", err)
			errs = append(errs, fmt.Errorf("vector token (project %d): %w", projectID, err))
			continue
		}
		pointsByAgent := map[int64][]Point{}
		jobsByAgent := map[int64][]Job{}
		for i, job := range batch {
			entry := entries[job.EntryID]
			pointsByAgent[entry.ApplicationID] = append(pointsByAgent[entry.ApplicationID], Point{
				EntryID: entry.ID, Version: entry.Version, Kind: entry.Kind, Text: entry.Content, Vector: embedding.Vectors[i],
			})
			jobsByAgent[entry.ApplicationID] = append(jobsByAgent[entry.ApplicationID], job)
		}
		for applicationID, points := range pointsByAgent {
			if err := s.store.UpsertAgentMemory(ctx, token, projectID, applicationID, space, points); err != nil {
				s.fail(ctx, jobsByAgent[applicationID], "vector write failed; retry", err)
				errs = append(errs, fmt.Errorf("write the vectors of agent %d (project %d): %w", applicationID, projectID, err))
				continue
			}
			if err := s.journal.Complete(ctx, jobsByAgent[applicationID]); err != nil {
				errs = append(errs, err)
				continue
			}
			closed += len(points)
		}
	}
	return closed, errors.Join(errs...)
}

func embeddingError(err error) error {
	if err == nil {
		return errors.New("the embedding returned a different number of vectors")
	}
	return err
}

// fail releases rows for another run after a backoff that grows with the
// attempts. It uses a context that outlives a shutdown for one short write, so
// the row is not left leased.
func (s *Service) fail(ctx context.Context, jobs []Job, class string, cause error) {
	if len(jobs) == 0 {
		return
	}
	attempts := 1
	for _, job := range jobs {
		attempts = max(attempts, job.Attempts)
	}
	if errors.Is(cause, ErrProjectScoped) {
		// Retrying sooner cannot help: the project has to get a model, or the
		// gateway to accept its calls. Park the rows at the longest backoff.
		attempts = 1 << 10
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.journal.Fail(writeCtx, jobs, class, Backoff(attempts)); err != nil {
		s.logger.Warn("agent memory: a vector journal row could not be released", "error", err)
	}
}

// Backoff is the wait after the nth failed run: 15 s, doubling, at most an hour.
func Backoff(attempts int) time.Duration {
	wait := baseBackoff
	for i := 1; i < attempts && wait < maxBackoff; i++ {
		wait *= 2
	}
	return min(wait, maxBackoff)
}
