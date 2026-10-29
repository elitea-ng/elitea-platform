package runevidence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Report bounds (run_step.proto).
const (
	MaxStepsPerReport   = 64
	MaxContentBytes     = 256 * 1024
	MaxSummaryBytes     = 8 * 1024
	MaxDocuments        = 100
	MaxCitedIDs         = 100
	MaxNodeIDBytes      = 256
	MaxNameBytes        = 512
	MaxToolkitIDBytes   = 128
	MaxIDBytes          = 256
	MaxQueryBytes       = 8 * 1024
	MaxFiltersBytes     = 8 * 1024
	MaxEvidenceFields   = 64
	MaxEvidenceFieldLen = 128
)

// ContentBucket is the platform-scoped object storage bucket of step content.
// Platform scope keeps it out of every project's artifact routes: a person
// cannot create a project bucket that reaches it.
const ContentBucket = "run-evidence"

// ContentInput is one content side as the worker reported it.
type ContentInput struct {
	Bytes        []byte
	MediaType    string
	Truncated    bool
	OriginalSize int64
	// Filtered is set by FilterFields, never by the worker.
	Filtered bool
}

// Document is one retrieval hit.
type Document struct {
	DocumentID string  `json:"document_id"`
	Source     string  `json:"source,omitempty"`
	Score      float64 `json:"score"`
	Rank       uint32  `json:"rank"`
}

// Retrieval is what a retrieval step searched and got back.
type Retrieval struct {
	IndexName string
	Query     string
	// FiltersJSON is a JSON object, or empty.
	FiltersJSON string
	TopK        uint32
	Documents   []Document
}

// StepInput is one step as the worker reported it.
type StepInput struct {
	StepID           string
	Kind             Kind
	StartedAt        time.Time
	EndedAt          time.Time
	NodeID           string
	Name             string
	ToolkitID        string
	SummaryJSON      string
	Retrieval        *Retrieval
	Inputs           *ContentInput
	Outputs          *ContentInput
	CitedStepIDs     []string
	CitedDocumentIDs []string
}

// ErrInvalidReport is a report outside the contract. It is not retryable.
var ErrInvalidReport = errors.New("invalid run step report")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidReport, fmt.Sprintf(format, args...))
}

// RunContext is what Main knows of the claimed run, read from the execution
// kernel, never from the worker.
type RunContext struct {
	RunID            string
	Generation       int64
	ProjectID        int64
	ApplicationID    *int64
	ReleaseID        *int64
	DefinitionDigest []byte
	Actor            string
	ActorUserID      *int64
	ConversationUUID string
	// MessageID is the chat response message the run produces, when it is a
	// chat turn.
	MessageID string
}

// Store is the run_steps side (repos.RunSteps).
type Store interface {
	// RunContext reads the claimed generation's run from the kernel.
	RunContext(ctx context.Context, executionID string, generation uint64) (RunContext, error)
	// EvidenceFields reads a toolkit's evidence_fields setting. set is false
	// when the toolkit sets none (keep everything) or does not resolve.
	EvidenceFields(ctx context.Context, projectID int64, toolkitID string) (fields []string, set bool, err error)
	// StoredStepIDs answers which of ids the run already stored.
	StoredStepIDs(ctx context.Context, runID string, ids []string) (map[string]bool, error)
	// Append stores the records in one transaction under the run's head
	// lock: it skips step_ids already stored, assigns seq and the chain
	// fields, and returns the number stored.
	Append(ctx context.Context, run RunContext, records []Record) (int, error)
	// JournalContent commits a journal entry per object key BEFORE the
	// objects are written (row-first): a key whose step row is never stored
	// is found by the step_content routine and its object deleted.
	JournalContent(ctx context.Context, run RunContext, entries []PendingContent) error
	// RequestAnchor marks the run's head for main.run_step_anchor. It is the
	// settlement path's only write and takes no security audit chain lock.
	RequestAnchor(ctx context.Context, runID string) error
}

// PendingContent is one journal entry: an object key about to be written
// for a step.
type PendingContent struct {
	Key    string
	StepID string
}

// ContentStore holds step content (object storage, ADR-0016).
type ContentStore interface {
	Put(ctx context.Context, key string, body []byte, mediaType string, digest []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, keys []string) error
}

// Service persists reported steps.
type Service struct {
	store   Store
	content ContentStore
	logger  *slog.Logger
	now     func() time.Time
}

// NewService builds the service. A nil content store keeps the digests and
// sizes and stores no bytes (the content reads as not_stored).
func NewService(store Store, content ContentStore, logger *slog.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("run evidence: a store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, content: content, logger: logger, now: time.Now}, nil
}

// ContentKey is a side's object key: per project and per run, and
// deterministic per step, so a retried report overwrites its own object
// instead of leaving an orphan.
func ContentKey(projectID int64, runID, stepID string, side Side) string {
	return strconv.FormatInt(projectID, 10) + "/runs/" + url.PathEscape(runID) + "/" + stepID + "/" + string(side)
}

// Report validates and stores one report of the claimed run. It returns the
// number of steps stored; steps whose step_id the run already stored are
// skipped. An ErrInvalidReport error refuses the whole report.
func (s *Service) Report(ctx context.Context, executionID string, generation uint64, steps []StepInput) (int, error) {
	if len(steps) > MaxStepsPerReport {
		return 0, invalid("more than %d steps", MaxStepsPerReport)
	}
	normalized := make([]normalizedStep, 0, len(steps))
	seen := make(map[string]bool, len(steps))
	for i, step := range steps {
		n, err := normalize(step)
		if err != nil {
			return 0, fmt.Errorf("step %d: %w", i, err)
		}
		if seen[n.record.StepID] {
			continue // a step repeated inside one report is stored once
		}
		seen[n.record.StepID] = true
		normalized = append(normalized, n)
	}
	if len(normalized) == 0 {
		return 0, nil
	}
	run, err := s.store.RunContext(ctx, executionID, generation)
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(normalized))
	for i, n := range normalized {
		ids[i] = n.record.StepID
	}
	stored, err := s.store.StoredStepIDs(ctx, run.RunID, ids)
	if err != nil {
		return 0, err
	}
	fieldsByToolkit := map[string]*[]string{}
	records := make([]Record, 0, len(normalized))
	pending := make([]pendingSides, 0, len(normalized))
	for _, n := range normalized {
		if stored[n.record.StepID] {
			continue
		}
		record := n.record
		record.RunID, record.Generation, record.ProjectID = run.RunID, run.Generation, run.ProjectID
		record.ApplicationID, record.ReleaseID, record.DefinitionDigest = run.ApplicationID, run.ReleaseID, run.DefinitionDigest
		record.Actor, record.ActorUserID, record.ConversationUUID = run.Actor, run.ActorUserID, run.ConversationUUID
		outputs := n.outputs
		if outputs != nil && (record.Kind == KindToolCall || record.Kind == KindRetrieval) && record.ToolkitID != "" {
			fields, ok := fieldsByToolkit[record.ToolkitID]
			if !ok {
				list, set, err := s.store.EvidenceFields(ctx, run.ProjectID, record.ToolkitID)
				if err != nil {
					return 0, err
				}
				if set {
					fields = &list
				}
				fieldsByToolkit[record.ToolkitID] = fields
			}
			if fields != nil {
				outputs = FilterFields(outputs, *fields)
			}
		}
		record.Inputs = s.sideMeta(run, record.StepID, SideInputs, n.inputs)
		record.Outputs = s.sideMeta(run, record.StepID, SideOutputs, outputs)
		records = append(records, record)
		pending = append(pending, pendingSides{inputs: n.inputs, outputs: outputs})
	}
	if len(records) == 0 {
		return 0, nil
	}
	// Row-first: the keys are journalled (committed) before any object is
	// written, so a persist that never lands leaves nothing untracked.
	var journal []PendingContent
	for _, r := range records {
		for _, key := range []string{r.Inputs.ObjectKey, r.Outputs.ObjectKey} {
			if key != "" {
				journal = append(journal, PendingContent{Key: key, StepID: r.StepID})
			}
		}
	}
	if err := s.store.JournalContent(ctx, run, journal); err != nil {
		return 0, err
	}
	for i, r := range records {
		if err := s.put(ctx, r.Inputs, pending[i].inputs); err != nil {
			return 0, err
		}
		if err := s.put(ctx, r.Outputs, pending[i].outputs); err != nil {
			return 0, err
		}
	}
	return s.store.Append(ctx, run, records)
}

type pendingSides struct {
	inputs, outputs *ContentInput
}

// RequestAnchor asks for the run's head to be anchored (on settlement); the
// main.run_step_anchor routine appends it to the security audit chain.
func (s *Service) RequestAnchor(ctx context.Context, executionID string) error {
	return s.store.RequestAnchor(ctx, executionID)
}

// sideMeta is what the row keeps of one side: digest, sizes and, when an
// object store is composed, the side's deterministic key (a retried report
// overwrites its own object).
func (s *Service) sideMeta(run RunContext, stepID string, side Side, content *ContentInput) ContentMeta {
	if content == nil {
		return ContentMeta{}
	}
	meta := ContentMeta{
		Size: int64(len(content.Bytes)), OriginalSize: max(content.OriginalSize, int64(len(content.Bytes))),
		Truncated: content.Truncated, Filtered: content.Filtered, MediaType: content.MediaType,
	}
	if len(content.Bytes) == 0 {
		meta.MediaType = ""
		return meta
	}
	digest := sha256.Sum256(content.Bytes)
	meta.Digest = digest[:]
	if s.content != nil {
		meta.ObjectKey = ContentKey(run.ProjectID, run.RunID, stepID, side)
	}
	return meta
}

// put writes one side's bytes after the key was journalled and before the
// row's transaction.
func (s *Service) put(ctx context.Context, meta ContentMeta, content *ContentInput) error {
	if meta.ObjectKey == "" || content == nil {
		return nil
	}
	if err := s.content.Put(ctx, meta.ObjectKey, content.Bytes, content.MediaType, meta.Digest); err != nil {
		return fmt.Errorf("store step content: %w", err)
	}
	return nil
}

type normalizedStep struct {
	record  Record
	inputs  *ContentInput
	outputs *ContentInput
}

func normalize(step StepInput) (normalizedStep, error) {
	parsed, err := uuid.Parse(step.StepID)
	if err != nil {
		return normalizedStep{}, invalid("step_id is not a UUID")
	}
	if !step.Kind.Valid() {
		return normalizedStep{}, invalid("unknown kind")
	}
	if step.StartedAt.IsZero() || step.EndedAt.IsZero() || step.EndedAt.Before(step.StartedAt) {
		return normalizedStep{}, invalid("started_at and ended_at must be set, in order")
	}
	for name, bound := range map[string][2]int{
		"node_id": {len(step.NodeID), MaxNodeIDBytes}, "name": {len(step.Name), MaxNameBytes},
		"toolkit_id": {len(step.ToolkitID), MaxToolkitIDBytes}, "summary_json": {len(step.SummaryJSON), MaxSummaryBytes},
		"cited_step_ids": {len(step.CitedStepIDs), MaxCitedIDs}, "cited_document_ids": {len(step.CitedDocumentIDs), MaxCitedIDs},
	} {
		if bound[0] > bound[1] {
			return normalizedStep{}, invalid("%s exceeds %d", name, bound[1])
		}
	}
	summary := []byte("{}")
	if step.SummaryJSON != "" {
		if summary, err = cleanObject([]byte(step.SummaryJSON)); err != nil {
			return normalizedStep{}, invalid("summary_json is not a bounded JSON object")
		}
	}
	record := Record{
		StepID: parsed.String(), Kind: step.Kind,
		StartedAt: step.StartedAt.UTC().Truncate(time.Microsecond), EndedAt: step.EndedAt.UTC().Truncate(time.Microsecond),
		NodeID: cleanString(step.NodeID), Name: cleanString(step.Name), ToolkitID: cleanString(step.ToolkitID),
		Summary: summary, CitedStepIDs: []string{}, CitedDocumentIDs: []string{},
	}
	for _, id := range step.CitedStepIDs {
		if len(id) > MaxIDBytes {
			return normalizedStep{}, invalid("a cited step id is too long")
		}
		record.CitedStepIDs = append(record.CitedStepIDs, cleanString(id))
	}
	for _, id := range step.CitedDocumentIDs {
		if len(id) > MaxIDBytes {
			return normalizedStep{}, invalid("a cited document id is too long")
		}
		record.CitedDocumentIDs = append(record.CitedDocumentIDs, cleanString(id))
	}
	if step.Retrieval != nil {
		if record.Retrieval, err = retrievalJSON(*step.Retrieval); err != nil {
			return normalizedStep{}, err
		}
	}
	inputs, err := normalizeContent(step.Inputs)
	if err != nil {
		return normalizedStep{}, fmt.Errorf("inputs: %w", err)
	}
	outputs, err := normalizeContent(step.Outputs)
	if err != nil {
		return normalizedStep{}, fmt.Errorf("outputs: %w", err)
	}
	return normalizedStep{record: record, inputs: inputs, outputs: outputs}, nil
}

func normalizeContent(in *ContentInput) (*ContentInput, error) {
	if in == nil {
		return nil, nil
	}
	copied := *in
	copied.Filtered = false
	content := &copied
	if len(content.Bytes) > MaxContentBytes {
		return nil, invalid("content exceeds %d bytes", MaxContentBytes)
	}
	if content.OriginalSize < 0 {
		return nil, invalid("original_size is negative")
	}
	switch content.MediaType {
	case MediaJSON, MediaText:
	case "":
		content.MediaType = MediaText
	default:
		return nil, invalid("media_type is application/json or text/plain")
	}
	return content, nil
}

func retrievalJSON(r Retrieval) ([]byte, error) {
	if len(r.Documents) > MaxDocuments {
		return nil, invalid("more than %d retrieved documents", MaxDocuments)
	}
	if len(r.IndexName) > MaxNameBytes || len(r.Query) > MaxQueryBytes || len(r.FiltersJSON) > MaxFiltersBytes {
		return nil, invalid("retrieval fields exceed their bounds")
	}
	type document struct {
		DocumentID string      `json:"document_id"`
		Source     string      `json:"source"`
		Score      json.Number `json:"score"`
		Rank       uint32      `json:"rank"`
	}
	out := struct {
		IndexName string          `json:"index_name"`
		Query     string          `json:"query"`
		Filters   json.RawMessage `json:"filters"`
		TopK      uint32          `json:"top_k"`
		Documents []document      `json:"documents"`
	}{IndexName: cleanString(r.IndexName), Query: cleanString(r.Query), TopK: r.TopK, Documents: []document{}}
	if r.FiltersJSON != "" {
		filters, err := cleanObject([]byte(r.FiltersJSON))
		if err != nil {
			return nil, invalid("retrieval filters are not a JSON object")
		}
		out.Filters = filters
	} else {
		out.Filters = json.RawMessage("{}")
	}
	for _, d := range r.Documents {
		if len(d.DocumentID) > MaxIDBytes || len(d.Source) > 2048 {
			return nil, invalid("a retrieved document's id or source is too long")
		}
		score := strconv.FormatFloat(d.Score, 'g', -1, 64)
		if _, err := strconv.ParseFloat(score, 64); err != nil || score == "NaN" || score == "+Inf" || score == "-Inf" {
			score = "0"
		}
		out.Documents = append(out.Documents, document{
			DocumentID: cleanString(d.DocumentID), Source: cleanString(d.Source), Score: json.Number(score), Rank: d.Rank,
		})
	}
	return json.Marshal(out)
}

// cleanObject decodes a JSON object, cleans its strings and re-encodes it.
func cleanObject(raw []byte) ([]byte, error) {
	value, err := decodeJSON(raw, true)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cleanJSONTree(value))
}

// FilterFields applies a toolkit's evidence_fields to a tool result: a JSON
// object keeps only the named top-level fields; anything else (text, an
// array, JSON the worker cut) cannot be filtered field by field and is not
// stored. Either way the side is marked filtered.
func FilterFields(content *ContentInput, fields []string) *ContentInput {
	out := &ContentInput{
		MediaType: content.MediaType, Truncated: content.Truncated, Filtered: true,
		OriginalSize: max(content.OriginalSize, int64(len(content.Bytes))),
	}
	if content.MediaType != MediaJSON {
		return out
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(content.Bytes, &object); err != nil || object == nil {
		return out
	}
	kept := make(map[string]json.RawMessage, len(fields))
	for _, name := range fields {
		if value, ok := object[name]; ok {
			kept[name] = value
		}
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return out
	}
	out.Bytes = encoded
	return out
}
