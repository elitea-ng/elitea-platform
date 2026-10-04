// Package requestlog records one row per request the gateway serves.
//
// # What it is for
//
// The question `gateway.llm_usage_events` cannot answer. That ledger is written
// from the billing delta, and a billing delta rides only a BILLED request — so
// a call refused by a budget, rejected by a policy, addressed to a model that
// does not resolve, or failed upstream leaves no trace in it. A log built over
// the ledger lists successes and no failures, which is the opposite of what an
// operator opens a log for.
//
// # NO REQUEST OR RESPONSE CONTENT, BY CONSTRUCTION
//
// There is no field on Record that a prompt, a completion or an upstream error
// string can reach. That is deliberate and it is structural rather than a rule
// somebody has to remember: a prompt is user-authored free text and carries
// whatever was pasted into it, and provider error messages routinely quote the
// offending fragment of the request back. The failure field is a CODE the
// gateway assigns from its own taxonomy.
//
// # It must never slow down or fail a request
//
// A log that adds a synchronous database write to the hot path makes every
// request slower and makes the platform unavailable whenever the log's table
// is. So Record() is a non-blocking send onto a bounded channel and nothing
// else; a background goroutine batches and writes.
//
// The buffer is BOUNDED and a full buffer DROPS, rather than blocking the
// caller or growing without limit. Both alternatives are worse than a gap in a
// log: blocking couples request latency to database latency — reintroducing the
// problem the queue exists to solve — and an unbounded buffer turns a database
// outage into an out-of-memory kill of the gateway itself.
//
// Drops are COUNTED and reported, because a silently lossy log is worse than no
// log: it invites an operator to conclude that traffic they cannot find did not
// happen. See Dropped().
package requestlog

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one served request, as the gateway saw it.
//
// Every field is a fact about the TRANSPORT or a value the gateway itself
// assigned. None of them is caller content.
type Record struct {
	OccurredAt time.Time
	// ProjectID and UserID are the resolved edge identity, empty when the
	// request never got far enough to have one. They are strings here and
	// integers in the table: the identity arrives as a header, and parsing it
	// at the boundary means a malformed value becomes NULL rather than an error
	// on the write path.
	ProjectID string
	UserID    string
	// Route is the chi route PATTERN, never the raw URL — a raw URL carries the
	// query string, which is another place a caller can put a secret.
	Route      string
	Method     string
	Status     int
	Duration   time.Duration
	Provider   string
	Model      string
	Streaming  bool
	ErrorCode  string
	PromptToks int64
	OutputToks int64
	// ExecutionID is the runtime execution this request was made from, empty
	// for every request that is not made from one.
	//
	// It does not break this struct's rule that no field is reachable by caller
	// content. The id is MINTED BY US — elitea_runtime.execution_jobs' own key —
	// and the edge validates its shape against a fixed charset and length bound
	// before it is signed into the identity tuple (elitea-main
	// internal/llmproxy/identity.go, executionIDFromHeader), so an unbounded or
	// free-text value cannot reach this field.
	//
	// It is stored RAW and resolved to an agent at read time. Denormalising an
	// agent id here would import execution_jobs' two disagreeing project columns
	// into a table whose whole point is having exactly one.
	ExecutionID string
	// CredentialOwner classifies who owns the provider credential that served
	// the request: CredentialOwnerProject (a client-owned credential of the
	// calling project), CredentialOwnerPlatform (a shared credential of the
	// public project), or "" when no credential served it. Legacy issue 6709.
	//
	// A classification the gateway assigns, never a credential id or name, so
	// this field keeps the struct's rule that no caller content reaches it.
	CredentialOwner string
	// CacheReadToks and CacheWriteToks are the prompt tokens the provider
	// reported as read from, and written to, its prompt cache. They are
	// SUBSETS of PromptToks, not additions to it.
	CacheReadToks  int64
	CacheWriteToks int64
}

// The values CredentialOwner can carry. Shared migration 0139 documents them on
// the column, and the analytics reads in elitea-main group on them.
const (
	CredentialOwnerProject  = "project"
	CredentialOwnerPlatform = "platform"
)

// Sink writes a batch of records. Implemented by the Postgres store; taken as
// an interface so the recorder's batching, dropping and shutdown behaviour is
// testable without a database.
type Sink interface {
	WriteBatch(ctx context.Context, records []Record) error
	// Prune deletes records older than the retention window and reports how
	// many it removed.
	Prune(ctx context.Context, olderThan time.Time) (int64, error)
}

const (
	// BufferSize bounds the in-memory queue. Sized for a burst rather than for
	// an outage: at any sustained rate the flusher keeps up, and if it cannot,
	// the honest outcome is a counted drop rather than unbounded growth.
	BufferSize = 4096

	// FlushInterval bounds how long a record waits before it is written. A log
	// an operator is watching during an incident is worth a write every second;
	// a longer interval would make the screen lie about what is happening now.
	FlushInterval = time.Second

	// FlushBatch is the largest single INSERT. It caps the statement size and
	// the time one write holds a connection.
	FlushBatch = 256

	// RetentionWindow is how long a row is kept. SHORTER than the billing
	// ledger's 400 days, deliberately: this table also holds every unbilled
	// request, so it grows faster.
	//
	// It is 92 days and not 30 because the analytics pages read this table,
	// and their widest preset is "Last 90d" (legacy issue 6879). With 30 days
	// that preset silently showed one month of data under a three-month
	// label. 92 days covers 90 calendar days plus the partial first and last
	// day a date-aligned window adds.
	//
	// A compiled constant rather than configuration, for the reason the
	// scheduler's own retention is one: no deployment should be able to turn a
	// log into an unbounded table by setting a variable.
	RetentionWindow = 92 * 24 * time.Hour

	// pruneInterval is how often the writer deletes expired rows. Hourly is far
	// more often than a 92-day window needs; it keeps each DELETE small, which
	// matters more than promptness here.
	pruneInterval = time.Hour
)

// Recorder is the non-blocking front of the log.
//
// A nil *Recorder is a working no-op, so a deployment without a database (the
// gateway's supported bootstrap posture) needs no branch at the call sites.
type Recorder struct {
	records chan Record
	sink    Sink
	logger  *slog.Logger
	now     func() time.Time

	dropped  atomic.Int64
	written  atomic.Int64
	failed   atomic.Int64
	stopOnce sync.Once
	done     chan struct{}
	stopped  chan struct{}
}

// New starts a recorder writing into sink. A nil sink returns nil, which every
// method tolerates.
func New(sink Sink, logger *slog.Logger) *Recorder {
	if sink == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Recorder{
		records: make(chan Record, BufferSize),
		sink:    sink,
		logger:  logger,
		now:     time.Now,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go r.run()
	return r
}

// Record queues one served request. It never blocks and never returns an error:
// the call site is the request path, and a log that can fail a request is a log
// that should not be on it.
func (r *Recorder) Record(record Record) {
	if r == nil {
		return
	}
	if record.OccurredAt.IsZero() {
		record.OccurredAt = r.now()
	}
	select {
	case r.records <- record:
	default:
		// The buffer is full: the flusher is behind, which in practice means
		// the database is. Dropping is the least-bad option — see the package
		// doc — and the count is what keeps it from being silent.
		r.dropped.Add(1)
	}
}

// Dropped reports how many records were discarded because the buffer was full.
//
// Exported so the metrics surface can publish it. A log with gaps must be able
// to say so: an operator who cannot find a request needs to know whether it did
// not happen or whether it was not written.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// Written and Failed report the flusher's outcomes, for the same reason.
func (r *Recorder) Written() int64 {
	if r == nil {
		return 0
	}
	return r.written.Load()
}

func (r *Recorder) Failed() int64 {
	if r == nil {
		return 0
	}
	return r.failed.Load()
}

// Stop drains what is queued and returns when the writer has finished.
//
// It drains rather than discarding: at shutdown the records in the buffer
// describe the requests served in the last second, which are exactly the ones
// an operator investigating a crash-loop wants. The drain is bounded by the
// caller's context so a wedged database cannot hold the process open.
func (r *Recorder) Stop(ctx context.Context) {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.done) })
	select {
	case <-r.stopped:
	case <-ctx.Done():
	}
}

func (r *Recorder) run() {
	defer close(r.stopped)

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	pruner := time.NewTicker(pruneInterval)
	defer pruner.Stop()

	batch := make([]Record, 0, FlushBatch)

	for {
		select {
		case record := <-r.records:
			batch = append(batch, record)
			if len(batch) >= FlushBatch {
				batch = r.flush(batch)
			}
		case <-ticker.C:
			batch = r.flush(batch)
		case <-pruner.C:
			r.prune()
		case <-r.done:
			// Drain what is already queued, then write it. The channel is
			// closed to no one, so this reads what is buffered and stops.
			for {
				select {
				case record := <-r.records:
					batch = append(batch, record)
					if len(batch) >= FlushBatch {
						batch = r.flush(batch)
					}
					continue
				default:
				}
				break
			}
			r.flush(batch)
			return
		}
	}
}

// flush writes the batch and returns an empty one to reuse.
//
// A FAILED write DISCARDS the batch rather than retrying. The records are
// already a second old, more are arriving, and a retry loop in front of a
// database that is refusing writes is how the buffer fills and starts dropping
// the newer records that describe the incident in progress. The failure is
// counted and logged instead.
func (r *Recorder) flush(batch []Record) []Record {
	if len(batch) == 0 {
		return batch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.sink.WriteBatch(ctx, batch); err != nil {
		r.failed.Add(int64(len(batch)))
		r.logger.Error("requestlog: batch write failed; the records are dropped",
			"records", len(batch), "err", err)
	} else {
		r.written.Add(int64(len(batch)))
	}
	return batch[:0]
}

func (r *Recorder) prune() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	removed, err := r.sink.Prune(ctx, r.now().Add(-RetentionWindow))
	if err != nil {
		r.logger.Error("requestlog: prune failed", "err", err)
		return
	}
	if removed > 0 {
		r.logger.Info("requestlog: pruned expired rows",
			"removed", removed, "retention", RetentionWindow.String())
	}
}
