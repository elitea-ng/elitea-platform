package indexregistry

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

// Metadata is the `metadata` object of one list element: the keys index_meta's
// cmetadata carries, in the shapes the web client reads
// (apps/elitea-web/.../indexes/api/indexesApi.ts). The typed counts the Rust
// runtime reports are added only once a run has reported them, so a row that
// has never completed is indistinguishable from an index_meta row.
//
// The response shape has no separate "total" and "last run" keys, so the
// existing keys are mapped as follows:
//
//   - `indexed` and `indexed_chunks` are the INDEX's totals: the number of
//     documents recorded in index_registry_documents and the sum of their
//     chunks (row.TotalDocuments, row.TotalChunks). An incremental run that
//     changed nothing leaves them as they were.
//   - `updated`, `failed_chunks` and `skipped` are the LAST run's counts.
//   - every `history` entry carries its own run's counts (`indexed` is the
//     documents that run wrote, `indexed_chunks` its chunks), and so does the
//     run's notification.
func Metadata(row Row) map[string]any {
	metadata := runMetadata(row)
	metadata["indexed"] = row.TotalDocuments
	if row.Stamped() {
		metadata["indexed_chunks"] = row.TotalChunks
	}
	history := make([]any, len(row.History))
	for i, entry := range row.History {
		history[i] = entry
	}
	metadata["history"] = history
	return metadata
}

// runMetadata is the top level with the run's own counts and without the
// history: what a history entry snapshots.
func runMetadata(row Row) map[string]any {
	metadata := map[string]any{
		"collection":           row.Name,
		"type":                 "index_meta",
		"indexed":              row.Indexed,
		"updated":              row.Updated,
		"state":                string(row.State),
		"index_configuration":  configurationObject(row.IndexConf),
		"created_on":           currentRunCreatedOn(row),
		"updated_on":           unixSeconds(row.UpdatedAt),
		"task_id":              nullableString(row.TaskID),
		"conversation_id":      nullableString(row.ConvID),
		"toolkit_id":           row.ToolkitID,
		"execution_id":         nullableText(row.ExecutionID),
		"execution_generation": nullableInt(row.ExecutionID != "", row.ExecutionGeneration),
		"index_generation":     row.IndexGeneration,
		"index_meta_id":        nullableText(row.MetaID),
		"correlation_id":       nullableText(row.CorrelationID),
	}
	if row.Error != nil {
		metadata["error"] = *row.Error
	}
	if row.Stamped() {
		metadata["indexed_chunks"] = row.Chunks
		metadata["failed_chunks"] = row.Failed
		metadata["skipped"] = skippedAny(row.Skipped)
		metadata["embedding_model"] = row.Model
		metadata["embedding_dimension"] = row.Dimension
	}
	return metadata
}

// configurationObject is the stored configuration, shared and not copied: a
// read only serializes it. A nil configuration reads as {}.
func configurationObject(configuration map[string]any) map[string]any {
	if configuration == nil {
		return map[string]any{}
	}
	return configuration
}

func skippedAny(skipped map[string]uint64) map[string]any {
	out := make(map[string]any, len(skipped))
	for reason, count := range skipped {
		out[reason] = count
	}
	return out
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(present bool, value int64) any {
	if !present {
		return nil
	}
	return value
}

// currentRunCreatedOn is when the newest run was admitted: the last history
// entry's created_on, as index_meta's top level carries the current run's.
func currentRunCreatedOn(row Row) any {
	if len(row.History) > 0 {
		if value, ok := row.History[len(row.History)-1]["created_on"]; ok {
			return value
		}
	}
	return unixSeconds(row.CreatedAt)
}

// snapshot is one history entry: the top level at that moment, without the
// history itself and without the chunking configuration, which index_meta
// repeats once per run and which made the list response grow linearly
// (issue #297).
func snapshot(row Row) map[string]any {
	entry := runMetadata(row)
	if configuration, ok := entry["index_configuration"].(map[string]any); ok {
		// Copied first: the map is the row's own, and the row keeps its
		// chunking configuration.
		configuration = copyObject(configuration)
		delete(configuration, "chunking_config")
		entry["index_configuration"] = configuration
	}
	return entry
}

// createdMarker is the permanent, run-neutral first entry of a new index.
func createdMarker(row Row) map[string]any {
	marker := snapshot(row)
	marker["state"] = string(StateCreated)
	marker["task_id"] = nil
	marker["conversation_id"] = nil
	for _, key := range []string{
		"execution_id", "execution_generation", "index_generation", "index_meta_id", "correlation_id",
	} {
		delete(marker, key)
	}
	return marker
}

// boundHistory keeps the newest entries within both caps. It discards a
// contiguous prefix, as the pgvector writer does.
func boundHistory(history []map[string]any) []map[string]any {
	if len(history) > MaxHistoryEntries {
		history = history[len(history)-MaxHistoryEntries:]
	}
	if len(history) == 0 {
		return history
	}
	last := len(history) - 1
	total := len("[]")
	first := last
	for index := last; index >= 0; index-- {
		entry, err := json.Marshal(history[index])
		if err != nil {
			break
		}
		next := total + len(entry)
		if index < last {
			next += len(",")
		}
		if index < last && next > MaxHistoryBytes {
			break
		}
		total = next
		first = index
	}
	return history[first:]
}

// matchesRun reports whether a history entry belongs to the run of the fence.
func matchesRun(entry map[string]any, executionID string, generation int64) bool {
	if entry["execution_id"] != executionID {
		return false
	}
	switch value := entry["execution_generation"].(type) {
	case int64:
		return value == generation
	case json.Number:
		parsed, err := value.Int64()
		return err == nil && parsed == generation
	case float64:
		return int64(value) == generation
	}
	return false
}

// The history reasons of a run that was still `in_progress` when it was found
// dead (see EndDeadRun).
const (
	// abandonedReason: its execution job is missing, or it SUCCEEDED and its
	// result was never applied within SettlingGrace.
	abandonedReason = "abandoned"
	// executionFailedReason: its job FAILED or was QUARANTINED before its
	// failed effect landed.
	executionFailedReason = "execution_failed"
)

// Effective is the row as its run's outcome will record it. A row whose run
// is dead (Dead: `in_progress`, its job over, its terminal effect not landed
// yet) reads as that outcome; any other row reads as stored. The list, the
// exact read and the scheduler read this, so a dead run never looks active.
func (r Row) Effective() Row {
	if !r.Dead() {
		return r
	}
	return EndDeadRun(r)
}

// EndDeadRun records the outcome of a dead run, derived from its execution job
// (RunStatus): a cancelled job is `cancelled` (as the queued cancel effect
// would record it), a failed or quarantined one is `failed`, and a missing job
// or a success whose result was never applied in time is `failed` with reason
// "abandoned". A row whose run is not dead is returned unchanged.
func EndDeadRun(row Row) Row {
	if !row.Dead() {
		return row
	}
	next := row.clone()
	switch {
	case !row.Job.SettledAt.IsZero():
		next.UpdatedAt = row.Job.SettledAt.UTC()
	case !row.ObservedAt.IsZero():
		next.UpdatedAt = row.ObservedAt.UTC()
	}
	reason := ""
	switch row.RunStatus() {
	case RunEndedCancelled:
		next.State = StateCancelled
		next.TaskID = nil
		next.Error = nil
	case RunEndedFailed:
		reason = executionFailedReason
		next.State = StateFailed
		next.Error = ptr("the indexing run failed")
	default:
		reason = abandonedReason
		next.State = StateFailed
		next.Error = ptr(abandonedReason)
	}
	finishRun(&next)
	if reason != "" {
		// finishRun replaced (or appended) the newest entry with a fresh map.
		next.History[len(next.History)-1]["reason"] = reason
	}
	return next
}

// StartRun is the admission initializer's transition. existing is nil when the
// index has no live row. It returns the row to store and whether anything
// changed; a retry of the same admitted run changes nothing.
//
// A row `in_progress` refuses a new run only while its run is active (its
// execution job is live, or SUCCEEDED with the result still being applied).
// When the run is dead its outcome is recorded first (EndDeadRun: cancelled,
// failed, or failed with reason "abandoned" when the job is missing) and the
// new run starts. The queued terminal effect of that run, when it lands later,
// names an older index generation and is refused as superseded, so it cannot
// overwrite the new run.
func StartRun(existing *Row, run indexingapp.RegistryInitialRun) (Row, bool, error) {
	if err := run.Validate(); err != nil {
		return Row{}, false, err
	}
	configuration, err := decodeObject(run.Configuration)
	if err != nil {
		return Row{}, false, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	admitted := run.AdmittedAt.UTC()
	generation := int64(run.Generation)
	indexGeneration := int64(run.IndexGeneration)

	var next Row
	if existing == nil {
		next = Row{
			ProjectID: run.ProjectID,
			ToolkitID: run.ToolkitID,
			Name:      run.IndexName,
			Skipped:   map[string]uint64{},
			CreatedAt: admitted,
		}
	} else {
		stored := *existing
		if stored.MetaID == run.MetaID {
			// A retry of the same admission must find the row exactly as the
			// first attempt left it, or it names a different run.
			if stored.IndexGeneration != indexGeneration || stored.ExecutionID != run.ExecutionID ||
				stored.ExecutionGeneration != generation {
				return Row{}, false, indexingapp.ErrCurrentIndexMetaConflict
			}
			return stored, false, nil
		}
		if stored.IndexGeneration > indexGeneration {
			return Row{}, false, indexingapp.ErrCurrentIndexMetaSuperseded
		}
		if stored.IndexGeneration == indexGeneration || !stored.CanStartNextRun() {
			return Row{}, false, indexingapp.ErrCurrentIndexMetaConflict
		}
		next = EndDeadRun(stored).clone()
	}

	next.State = StateInProgress
	next.TaskID = ptr(run.ExecutionID)
	next.Error = nil
	next.ConvID = nil
	next.IndexConf = configuration
	// The counts describe the run in flight, as index_meta's initial write
	// resets them. The embedding stamp is the index's, and is kept.
	next.Indexed, next.Updated, next.Chunks, next.Failed = 0, 0, 0, 0
	next.Skipped = map[string]uint64{}
	next.ExecutionID = run.ExecutionID
	next.ExecutionGeneration = generation
	next.IndexGeneration = indexGeneration
	next.MetaID = run.MetaID
	next.CorrelationID = run.CorrelationID
	next.UpdatedAt = admitted

	runEntry := func() map[string]any {
		entry := snapshot(next)
		entry["created_on"] = unixSeconds(admitted)
		return entry
	}
	if existing == nil {
		next.History = []map[string]any{createdMarker(next), runEntry()}
	} else {
		next.History = append(next.History, runEntry())
	}
	next.History = boundHistory(next.History)
	return next, true, nil
}

func fenceMatches(row Row, executionID string, generation, indexGeneration int64, metaID string) error {
	if row.IndexGeneration > indexGeneration {
		return indexingapp.ErrCurrentIndexMetaSuperseded
	}
	if row.ExecutionID != executionID || row.ExecutionGeneration != generation ||
		row.IndexGeneration != indexGeneration || (metaID != "" && row.MetaID != metaID) {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}

// finishRun rewrites the newest history entry for the run to the row's new
// top level. When the run has no entry (the initializer never ran for it) one
// is appended, which is what the pgvector writer does when "the SDK never
// started".
func finishRun(row *Row) {
	entry := snapshot(*row)
	if len(row.History) > 0 && matchesRun(row.History[len(row.History)-1], row.ExecutionID, row.ExecutionGeneration) {
		if created, ok := row.History[len(row.History)-1]["created_on"]; ok {
			entry["created_on"] = created
		}
		row.History[len(row.History)-1] = entry
	} else {
		row.History = append(row.History, entry)
	}
	row.History = boundHistory(row.History)
}

// ApplyTerminal applies a failed or cancelled transition. The first terminal
// transition of a run wins: a row already at rest is returned unchanged.
func ApplyTerminal(row Row, terminal indexingapp.RegistryTerminal) (Row, bool, error) {
	if err := terminal.Validate(); err != nil {
		return Row{}, false, err
	}
	if err := fenceMatches(
		row, terminal.ExecutionID, int64(terminal.Generation), int64(terminal.IndexGeneration), terminal.MetaID,
	); err != nil {
		return Row{}, false, err
	}
	if row.State != StateInProgress {
		return row, false, nil
	}
	next := row.clone()
	next.UpdatedAt = terminal.OccurredAt.UTC()
	switch terminal.State {
	case indexingapp.CurrentIndexMetaFailed:
		next.State = StateFailed
		next.Error = ptr(terminal.SafeError)
	case indexingapp.CurrentIndexMetaCancelled:
		next.State = StateCancelled
		next.TaskID = nil
		next.Error = nil
	default:
		return Row{}, false, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	finishRun(&next)
	return next, true, nil
}

// Result is the typed terminal result of one run, as the output projection
// hands it over (outputapp.IndexIngestSummary plus the run it belongs to).
type Result struct {
	ExecutionID string
	Generation  uint64
	OccurredAt  time.Time
	Summary     outputapp.IndexIngestSummary
}

// DimensionMismatchError is the failure a result gets when it names another
// embedding space than the index was stamped with.
type DimensionMismatchError struct {
	StampedModel     string
	StampedDimension int32
	ResultModel      string
	ResultDimension  int32
}

func (e *DimensionMismatchError) Error() string {
	if e.StampedDimension != e.ResultDimension {
		return fmt.Sprintf(
			"embedding dimension changed from %d (%s) to %d (%s): an index keeps one embedding space, re-index into a new index to change the model",
			e.StampedDimension, e.StampedModel, e.ResultDimension, e.ResultModel,
		)
	}
	return fmt.Sprintf(
		"embedding model changed from %s to %s (both %d-dimensional): an index keeps one embedding space, re-index into a new index to change the model",
		e.StampedModel, e.ResultModel, e.StampedDimension,
	)
}

// stampsEmbedding reports whether a result may stamp (or must match) the
// index's embedding space: it indexed something. An error result, and a result
// that wrote no chunk, say nothing about the space the vectors live in, so a
// failed first run never locks the index to a model it did not finish using.
func stampsEmbedding(summary outputapp.IndexIngestSummary) bool {
	return summary.EmbeddingDimension != 0 && summary.IndexedChunks > 0 &&
		(summary.Status == outputapp.IndexIngestStatusOK ||
			summary.Status == outputapp.IndexIngestStatusPartlyIndexed)
}

// CheckEmbedding returns the mismatch a result would be turned into a failure
// for: it would stamp the index, the index is already stamped, and the two
// differ. It reads nothing but its arguments.
func CheckEmbedding(row Row, summary outputapp.IndexIngestSummary) *DimensionMismatchError {
	if !stampsEmbedding(summary) || !row.Stamped() {
		return nil
	}
	dimension := int32(summary.EmbeddingDimension)
	if row.Dimension == dimension && row.Model == summary.EmbeddingModel {
		return nil
	}
	return &DimensionMismatchError{
		StampedModel: row.Model, StampedDimension: row.Dimension,
		ResultModel: summary.EmbeddingModel, ResultDimension: dimension,
	}
}

// NoTypedSummaryReason is the failure of a rust-mode result that carries no
// inline IndexIngestSummaryV1 (only an artifact). The Rust worker always sends
// the typed summary; one without it cannot stamp, count or be trusted as a
// success, so the registry records the run failed instead.
const NoTypedSummaryReason = "the index result carries no typed summary"

// failureSummary is a failed terminal summary with a safe message.
func failureSummary(message string) outputapp.IndexIngestSummary {
	return outputapp.IndexIngestSummary{
		Status:        outputapp.IndexIngestStatusError,
		Message:       message,
		TerminalState: outputapp.IndexIngestTerminalFailed,
	}
}

// FailedSummary is the summary a mismatching result is recorded as.
func (e *DimensionMismatchError) FailedSummary() outputapp.IndexIngestSummary {
	return failureSummary(e.Error())
}

// RecordedSummary is what the registry records for a result reported against
// this row, and what the user-facing surfaces (the notification, the
// activity) describe: the worker's summary, or a failure when the result has
// no typed summary (NoTypedSummaryReason) or names another embedding space
// than the index was stamped with (DimensionMismatchError). overridden reports
// the second case.
//
// This is a BACKSTOP. The worker is told the expected embedding space in the
// index command (IndexIngestCommandV1.expected_embedding_model/_dimension) and
// must fail before writing a vector when it differs; a worker that ignores
// that reaches this check. The output plane has no typed refusal that makes a
// worker settle FAILED (only a cancellation or deadline winner can replace a
// terminal frame), so the settlement, the stored projection and the replay
// event keep the worker's own outcome while the registry and the notification
// say failed. That inconsistency is deliberate and bounded to a misbehaving
// worker.
func RecordedSummary(row Row, summary outputapp.IndexIngestSummary) (recorded outputapp.IndexIngestSummary, overridden bool) {
	if summary == (outputapp.IndexIngestSummary{}) {
		return failureSummary(NoTypedSummaryReason), true
	}
	if mismatch := CheckEmbedding(row, summary); mismatch != nil {
		return mismatch.FailedSummary(), true
	}
	return summary, false
}

// ApplyResult applies the typed result of a run that reached the output
// projection. It returns the row to store and whether anything changed. A row
// not `in_progress` is returned unchanged: a cancel or an earlier terminal
// transition has already won.
//
// The result is recorded as RecordedSummary says: the first result that
// indexed something (status ok or partly_indexed, at least one chunk) stamps
// the model and dimension (ADR-0030 decision 2); a later one with another
// dimension, or another model, touches neither the counts nor the stamp and
// fails the run with a message that says so, and so does a result without a
// typed summary.
func ApplyResult(row Row, result Result) (next Row, changed bool, err error) {
	if result.ExecutionID == "" || result.Generation == 0 ||
		result.Generation > math.MaxInt64 || result.OccurredAt.IsZero() {
		return Row{}, false, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	if result.Summary != (outputapp.IndexIngestSummary{}) {
		if err := result.Summary.Validate(); err != nil {
			return Row{}, false, indexingapp.ErrCurrentIndexMetaInitializationInvalid
		}
	}
	if row.ExecutionID != result.ExecutionID || row.ExecutionGeneration != int64(result.Generation) {
		return Row{}, false, indexingapp.ErrCurrentIndexMetaConflict
	}
	if row.State != StateInProgress {
		return row, false, nil
	}
	summary, overridden := RecordedSummary(row, result.Summary)
	next = row.clone()
	next.UpdatedAt = result.OccurredAt.UTC()
	if overridden {
		next.State = StateFailed
		next.Error = ptr(summary.Message)
		finishRun(&next)
		return next, true, nil
	}
	if stampsEmbedding(summary) && !next.Stamped() {
		next.Model = summary.EmbeddingModel
		next.Dimension = int32(summary.EmbeddingDimension)
		next.Collection = CollectionName(summary.EmbeddingModel, next.Dimension)
	}

	if summary.HasTypedResult() {
		next.Indexed = int64(summary.IndexedDocuments)
	} else {
		next.Indexed = int64(summary.Indexed)
	}
	next.Updated = int64(summary.Updated)
	next.Chunks = int64(summary.IndexedChunks)
	next.Failed = int64(summary.FailedChunks)
	next.Skipped = summary.Skipped()

	switch summary.Status {
	case outputapp.IndexIngestStatusError:
		next.State = StateFailed
		next.Error = ptr(summary.Message)
	case outputapp.IndexIngestStatusPartlyIndexed:
		next.State = StatePartlyIndexed
		next.Error = nil
	default:
		next.State = StateCompleted
		next.Error = nil
	}
	finishRun(&next)
	return next, true, nil
}

// VerifyManualStop checks the evidence the manual-Stop cleanup acts on: the
// row is cancelled for exactly the stopped run.
func VerifyManualStop(row Row, stop indexingapp.RegistryManualStop) error {
	if err := stop.Validate(); err != nil {
		return err
	}
	if err := fenceMatches(
		row, stop.ExecutionID, int64(stop.Generation), int64(stop.IndexGeneration), stop.MetaID,
	); err != nil {
		return err
	}
	if row.State != StateCancelled || row.TaskID != nil {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}

// RecordScheduledFailure appends the failed-schedule entry. It is idempotent on
// effectID and leaves a run that is still in progress alone: a schedule that
// could not start must not overwrite the state of a run that did.
func RecordScheduledFailure(row Row, effectID, safeReason string, occurredAt time.Time) (Row, bool) {
	for _, entry := range row.History {
		if entry["schedule_effect_id"] == effectID {
			return row, false
		}
	}
	if row.State == StateInProgress {
		return row, false
	}
	next := row.clone()
	next.State = StateFailed
	next.Error = ptr(safeReason)
	next.UpdatedAt = occurredAt.UTC()
	entry := snapshot(next)
	entry["schedule_effect_id"] = effectID
	next.History = boundHistory(append(next.History, entry))
	return next, true
}
