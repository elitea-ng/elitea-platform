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
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
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

	// RunActive is read, never stored: it is true when the execution job the
	// row names (TaskID, ExecutionGeneration) exists and is not terminal. It is
	// the only liveness signal of a run (see Active). The repository fills it
	// on every read; a Row built any other way is not active.
	RunActive bool
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
// AND the execution job it names is not terminal. A row `in_progress` whose job
// is terminal or missing is a dead run (its worker died or its result was never
// applied), not an active one. No timestamp is involved, so a healthy run that
// reports rarely is never mistaken for a dead one.
func (r Row) Active() bool { return r.State == StateInProgress && r.RunActive }

// Abandoned reports an `in_progress` row whose run is dead.
func (r Row) Abandoned() bool { return r.State == StateInProgress && !r.RunActive }

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

// CollectionName is ADR-0031 decision 2's `emb_<model-slug>_<dimension>`: one
// vector collection per embedding space. The slug is the lower-cased model name
// with every run of other characters replaced by one underscore, bounded so the
// name stays well inside a collection-name limit.
func CollectionName(model string, dimension int32) string {
	slug := strings.Trim(collectionSlugPattern.ReplaceAllString(strings.ToLower(model), "_"), "_")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "_")
	}
	if slug == "" {
		slug = "model"
	}
	return "emb_" + slug + "_" + itoa(int64(dimension))
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
