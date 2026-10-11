// Package indexregistry is the application layer of the index registry
// (ADR-0031 V1, ADR-0030 decision 2): the Rust indexing runtime's replacement
// for the `index_meta` embedding row in a project's pgvector database.
//
// elitea-main is the only writer. Every transition here is a pure function of
// the stored Row and one fenced input (an admitted run, a terminal effect, a
// typed result), so the Postgres repository only has to lock a row, call the
// function and store what it returns, and the rules are tested without a
// database. The routes (list, delete, configuration save) and the schedule
// kernel read through Service, which answers in the exact shapes the Python
// path answers in: the web client's index list does not change.
package indexregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// State is the closed set index_meta carries today. `scheduled_reindex` is a
// notification marker and never a row state.
type State string

const (
	StateCreated       State = "created"
	StateInProgress    State = "in_progress"
	StateCompleted     State = "completed"
	StatePartlyIndexed State = "partly_indexed"
	StateFailed        State = "failed"
	StateCancelled     State = "cancelled"
)

func (s State) Valid() bool {
	switch s {
	case StateCreated, StateInProgress, StateCompleted, StatePartlyIndexed, StateFailed, StateCancelled:
		return true
	}
	return false
}

const (
	// MaxHistoryEntries is the cap index_meta_writer.go applies
	// (indexing.MaxCurrentIndexMetaHistoryEntries): history gains one entry per
	// run, and the oldest go first.
	MaxHistoryEntries = indexingapp.MaxCurrentIndexMetaHistoryEntries
	// MaxHistoryBytes is the same byte budget the pgvector writer keeps: a
	// quarter of the row cap, so a worst-case encoding still fits the row.
	MaxHistoryBytes = indexingapp.MaxCurrentInitialIndexMetaBytes / 4
)

var (
	ErrNotFound = errors.New("index registry row was not found")
	// ErrActiveRun reports a delete of an index whose run is still active: the
	// execution job recorded on the row is not terminal. The vectors of a run
	// that is still writing would be orphaned, so the caller stops it first.
	ErrActiveRun = errors.New("index registry row has an active run")
	// ErrInvalid reports a row or input this layer refuses to store.
	ErrInvalid = errors.New("invalid index registry input")
)

// Row is one index_registry row. History entries are the same run snapshots the
// Python path stores; the other fields are the typed columns.
type Row struct {
	IndexID    string
	ProjectID  int32
	ToolkitID  int32
	Name       string
	State      State
	TaskID     *string
	Error      *string
	ConvID     *string
	History    []map[string]any
	IndexConf  map[string]any
	Indexed    int64 // indexed_documents
	Updated    int64 // updated_documents
	Chunks     int64 // indexed_chunks
	Failed     int64 // failed_chunks
	Skipped    map[string]uint64
	Model      string // embedding_model, "" until stamped
	Dimension  int32  // embedding_dimension, 0 until stamped
	Collection string // "" until stamped

	ExecutionID         string
	ExecutionGeneration int64
	IndexGeneration     int64
	MetaID              string
	CorrelationID       string

	CreatedAt time.Time
	UpdatedAt time.Time

	// Job is the execution job the row names (TaskID's execution at
	// ExecutionGeneration), read with the row and never stored. It is the only
	// liveness signal of a run (see RunStatus). The repository fills it, and
	// ObservedAt, on every read; a Row built any other way has no job.
	Job RunJob
	// ObservedAt is the database clock at the read, against which the settling
	// grace (SettlingGrace) is measured.
	ObservedAt time.Time

	// TotalDocuments and TotalChunks are the index's totals: the number of
	// index_registry_documents rows and the sum of their chunk_count. They are
	// read by the list and exact reads only, and never stored on the row; the
	// Indexed/Updated/Chunks/Failed columns above are the LAST run's counts.
	TotalDocuments int64
	TotalChunks    int64
}

// RunJob is the execution job of a row's run, as the repository read it.
type RunJob struct {
	// Found is false when no execution_jobs row has the row's fence.
	Found        bool
	State        executiondomain.JobState
	DesiredState string // execution_jobs.desired_state: RUNNING, CANCELLED or DRAINING
	// SettledAt is when the job reached its terminal state; zero when it has
	// not, or when the job does not record it.
	SettledAt time.Time
}

// SettlingGrace bounds how long a run whose job SUCCEEDED may stay
// `in_progress` while its result is applied. Past it, the run is abandoned:
// nothing will apply that result any more.
const SettlingGrace = 10 * time.Minute

const desiredCancelled = "CANCELLED"

// RunStatus is the liveness of a row's run, derived from its execution job.
type RunStatus string

const (
	// RunAtRest: the row is not `in_progress`; no run owns it.
	RunAtRest RunStatus = "at_rest"
	// RunLive: the job is not terminal.
	RunLive RunStatus = "live"
	// RunSettling: the job SUCCEEDED and its result has not been applied yet,
	// within SettlingGrace of the job's terminal time.
	RunSettling RunStatus = "settling"
	// RunEndedCancelled: the job was cancelled (state CANCELLED, or a terminal
	// failure while its desired state was CANCELLED); its cancelled effect has
	// not landed on the row yet.
	RunEndedCancelled RunStatus = "cancelled"
	// RunEndedFailed: the job FAILED or was QUARANTINED; its failed effect has
	// not landed on the row yet.
	RunEndedFailed RunStatus = "failed"
	// RunAbandoned: the job is missing, or it SUCCEEDED and its result was not
	// applied within SettlingGrace.
	RunAbandoned RunStatus = "abandoned"
)

// RunStatus derives the run's liveness from the job the row names. No
// timestamp of the row itself is involved, so a healthy run that reports
// rarely is never mistaken for a dead one.
func (r Row) RunStatus() RunStatus {
	if r.State != StateInProgress {
		return RunAtRest
	}
	if !r.Job.Found {
		return RunAbandoned
	}
	switch r.Job.State {
	case executiondomain.JobSucceeded:
		if !r.Job.SettledAt.IsZero() &&
			(r.ObservedAt.IsZero() || r.ObservedAt.Before(r.Job.SettledAt.Add(SettlingGrace))) {
			return RunSettling
		}
		return RunAbandoned
	case executiondomain.JobCancelled:
		return RunEndedCancelled
	case executiondomain.JobFailed, executiondomain.JobQuarantined:
		if r.Job.DesiredState == desiredCancelled {
			return RunEndedCancelled
		}
		return RunEndedFailed
	}
	// PENDING, DISPATCHED, CLAIMED, RUNNING, SETTLING (and any state this
	// code does not know, which is never treated as over).
	return RunLive
}

// DocumentVersion is one indexed document's (document_key, version): the unit
// of incremental sync (ADR-0030 decision 3). A run lists the source, skips a
// document whose version is unchanged, rewrites a changed one by deleting its
// chunks by key, and forgets one that disappeared.
type DocumentVersion struct {
	Key        string
	Version    string
	ChunkCount int32
}

func (d DocumentVersion) Validate() error {
	if d.Key == "" || len(d.Key) > 4096 || len(d.Version) > 1024 || d.ChunkCount < 0 {
		return ErrInvalid
	}
	return nil
}

// Tombstone is a deleted index waiting for its vectors to be deleted.
type Tombstone struct {
	IndexID   string
	ProjectID int32
	ToolkitID int32
	Name      string
	DeletedAt time.Time
	// Attempts is how many deletions the sweeper has tried and failed.
	Attempts int32
}

// Stamped reports whether the first terminal result has stamped the embedding
// space.
func (r Row) Stamped() bool { return r.Dimension != 0 }

// Active reports whether a run owns the row right now: the row is `in_progress`
// AND its execution job is live, or SUCCEEDED with its result still being
// applied (RunSettling). A row `in_progress` whose job ended otherwise is a
// dead run, not an active one.
func (r Row) Active() bool {
	status := r.RunStatus()
	return status == RunLive || status == RunSettling
}

// Dead reports an `in_progress` row whose run is over although the row has not
// recorded it yet: its terminal effect is still queued, or its job is missing.
func (r Row) Dead() bool { return r.State == StateInProgress && !r.Active() }

// CanStartNextRun is index_meta's rule, with liveness from the job: a row at
// rest may start its next generation, and so may a row whose run is dead. Only
// an active run owns the row.
func (r Row) CanStartNextRun() bool { return !r.Active() }

// clone is the copy a transition mutates. History entries and the
// configuration are never changed in place by a transition (an entry is
// replaced or appended, the configuration is replaced as a whole), so the
// containers are copied and the values are shared; reads do not clone at all.
func (r Row) clone() Row {
	out := r
	out.History = append(make([]map[string]any, 0, len(r.History)+1), r.History...)
	out.Skipped = make(map[string]uint64, len(r.Skipped))
	for reason, count := range r.Skipped {
		out.Skipped[reason] = count
	}
	return out
}

// copyObject is a shallow copy, for the one reader that removes a key.
func copyObject(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// decodeObject decodes one JSON object keeping numbers exact.
func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrInvalid
	}
	return object, nil
}

// unixSeconds is the representation index_meta uses for created_on and
// updated_on: seconds since the epoch, fractional.
func unixSeconds(value time.Time) json.Number {
	value = value.UTC()
	seconds := float64(value.Unix()) + float64(value.Nanosecond())/float64(time.Second)
	encoded, _ := json.Marshal(seconds)
	return json.Number(encoded)
}

func ptr(value string) *string { return &value }

var collectionSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

const (
	// maxVectorSpaceSlugBytes is elitea-vector's model-slug limit
	// (services/elitea-vector/src/layout.rs: ^[a-z0-9][a-z0-9-]{0,62}$).
	maxVectorSpaceSlugBytes = 63
	// collectionHashHex is how much of the model's SHA-256 the slug carries.
	collectionHashHex = 12
	// maxReadableSlugBytes leaves room for "-" and the hash.
	maxReadableSlugBytes = maxVectorSpaceSlugBytes - 1 - collectionHashHex
)

// VectorSpaceSlug is the model slug of an embedding space as elitea-vector
// accepts it (lower-case letters, digits and '-', at most 63 bytes, starting
// with a letter or digit): a human-readable part, then '-' and the first 12 hex
// digits of the SHA-256 of the EXACT model string. The readable part is the
// lower-cased model with every run of other characters replaced by one '-',
// truncated; it is for people. The hash is what makes the name lossless: two
// models that differ in case, punctuation or only after the truncation point
// still get different slugs.
func VectorSpaceSlug(model string) string {
	readable := strings.Trim(collectionSlugPattern.ReplaceAllString(strings.ToLower(model), "-"), "-")
	if len(readable) > maxReadableSlugBytes {
		readable = strings.TrimRight(readable[:maxReadableSlugBytes], "-")
	}
	if readable == "" {
		readable = "model"
	}
	sum := sha256.Sum256([]byte(model))
	return readable + "-" + hex.EncodeToString(sum[:])[:collectionHashHex]
}

// CollectionName is ADR-0031 decision 2's one vector collection per embedding
// space, in elitea-vector's own form: `emb_<slug>_<dimension>`, where slug is
// VectorSpaceSlug(model). For example "text-embedding-3-small" at 1536 is
// `emb_text-embedding-3-small-<h>_1536`. elitea-vector parses the dimension
// after the last '_' and validates the slug, so the name round-trips through
// its Space::from_collection.
func CollectionName(model string, dimension int32) string {
	return "emb_" + VectorSpaceSlug(model) + "_" + itoa(int64(dimension))
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
