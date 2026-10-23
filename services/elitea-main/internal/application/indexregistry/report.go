package indexregistry

import (
	"encoding/json"
	"fmt"
	"sort"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

// ReportVersion is the SDK's REPORT_VERSION
// (elitea_sdk/tools/base_indexer_toolkit.py): the schema the web client's
// normalizeIndexingReport reads from index_meta's `report` key.
const ReportVersion = 1

// Report kinds, in the order the SDK lists the categories.
const (
	kindIndexed    = "indexed"
	kindSkipped    = "skipped"
	kindNotIndexed = "not_indexed"
	kindFailed     = "failed"
)

var reportKindOrder = []string{kindIndexed, kindSkipped, kindNotIndexed, kindFailed}

// reportGroupSpec is the report group one Rust skip reason lands in.
type reportGroupSpec struct {
	kind   string
	reason string
	label  string
}

// skipReasonGroups maps the closed skip reasons of the Rust indexing worker
// (IndexIngestSummaryV1.skipped; services/elitea-worker-rust pipeline.rs) onto
// the SDK report's categories and groups. Reasons that share a group (the SDK
// has one "read_error" for both a failed fetch and an unreadable file) are
// summed. The Rust runtime records counts, never names, so a group has no
// sampled items.
var skipReasonGroups = map[string]reportGroupSpec{
	"unsupported_type":   {kindNotIndexed, "unsupported_format", "Unsupported format"},
	"empty":              {kindSkipped, "empty", "Contained no indexable content"},
	"unreadable":         {kindFailed, "read_error", "Could not be read"},
	"fetch_failed":       {kindFailed, "read_error", "Could not be read"},
	"rate_limited":       {kindFailed, "rate_limited", "Rate limited by the source; a later run retries"},
	"extract_failed":     {kindFailed, "processing_error", "Could not be processed"},
	"extract_timeout":    {kindFailed, "extract_timeout", "Timed out while extracting content"},
	"embedding_failed":   {kindFailed, "embedding_failed", "Embedding failed"},
	"model_failed":       {kindFailed, "model_failed", "The chat model failed"},
	"too_large":          {kindNotIndexed, "too_large", "Too large to index"},
	"too_many_pages":     {kindNotIndexed, "too_many_pages", "Too many pages to index"},
	"encrypted":          {kindNotIndexed, "encrypted", "Encrypted or password-protected"},
	"needs_model":        {kindNotIndexed, "needs_model", "Needs a chat model the chunker did not have"},
	"no_model":           {kindNotIndexed, "no_model", "No chat model configured for this step"},
	"model_cap_exceeded": {kindNotIndexed, "model_cap_exceeded", "Needs more model calls than the per-run cap"},
	"vision_refused":     {kindNotIndexed, "vision_refused", "The model refused the image"},
	"reply_unusable":     {kindNotIndexed, "reply_unusable", "The model's reply could not be used"},
	// Benign for the run status: the model spent max_tokens on reasoning; the
	// same request gets the same cut, so it is final until max_tokens is raised.
	"reasoning_exhausted_tokens": {kindNotIndexed, "reasoning_exhausted_tokens", "The model spent its max_tokens on reasoning; raise max_tokens"},
	"too_many_pictures":          {kindNotIndexed, "too_many_pictures", "Too many pictures to describe"},
}

// unknownReasonLabel is the label of a reason this table does not know. The raw
// token is carried beside it as the group's `code`, never as the label.
const unknownReasonLabel = "Not indexed (other)"

// groupForSkipReason is the group of a skip reason. known is false for a
// reason this table does not know (a newer worker): it is never dropped, it
// lands in not_indexed under its own token as the group key, with a generic
// label.
func groupForSkipReason(reason string) (spec reportGroupSpec, known bool) {
	if spec, ok := skipReasonGroups[reason]; ok {
		return spec, true
	}
	return reportGroupSpec{kindNotIndexed, reason, unknownReasonLabel}, false
}

// Run statuses of a report. ok, partly_indexed and error are the SDK's; the web
// client also knows cancelled.
const (
	statusOK            = "ok"
	statusPartlyIndexed = "partly_indexed"
	statusError         = "error"
	statusCancelled     = "cancelled"
)

const (
	// runRecordKey is where a history entry keeps its runRecord.
	runRecordKey = "run_record"
	// cancelledMessage is the message of a run that was stopped.
	cancelledMessage = "Indexing was stopped before it finished."
)

// runRecord is what a history entry stores of a run's report: only the parts
// that vary. The labels, the empty categories and the other constants are
// added when the metadata is served (Report), so a history of many runs stays
// small. A record exists only for a run that ended: it is written by the
// transition that ended it (a typed result, a failure, a cancel, a dead run, a
// failed schedule), so a row's report is gated on "this run ended", never on
// the index's embedding stamp.
type runRecord struct {
	Status string `json:"status"`
	// Indexed is the documents THIS run wrote.
	Indexed uint64 `json:"indexed"`
	// Unchanged is the documents the run listed and found already indexed. It
	// is zero for a run that did not complete.
	Unchanged    uint64            `json:"unchanged,omitempty"`
	Skipped      map[string]uint64 `json:"skipped,omitempty"`
	FailedChunks uint64            `json:"failed_chunks,omitempty"`
	Errors       []string          `json:"errors,omitempty"`
}

// failedRun reports a run that did not complete (error or cancelled).
func (r runRecord) failedRun() bool { return r.Status == statusError || r.Status == statusCancelled }

// failureRecord is the record of a run that ended without a typed result, or
// with one that was refused: nothing was counted, the message says why.
func failureRecord(status, message string) runRecord {
	record := runRecord{Status: status}
	if message != "" {
		record.Errors = []string{message}
	}
	return record
}

func cancelledRecord() runRecord { return failureRecord(statusCancelled, cancelledMessage) }

// resultRecord is the record of a run whose typed result was applied.
//
// Unchanged is the worker's exact count of listed-but-unchanged documents
// (IndexIngestSummary.UnchangedPresent). A worker that does not report it (an
// older one) leaves the line out: nothing is derived from the totals. A run
// that did not complete says nothing about what was left alone.
func resultRecord(summary outputapp.IndexIngestSummary, status string) runRecord {
	indexed := summary.Indexed
	if summary.HasTypedResult() {
		indexed = summary.IndexedDocuments
	}
	record := runRecord{
		Status:       status,
		Indexed:      indexed,
		Skipped:      summary.Skipped(),
		FailedChunks: summary.FailedChunks,
	}
	switch status {
	case statusError:
		if summary.Message != "" {
			record.Errors = []string{summary.Message}
		}
	case statusCancelled:
		record.Errors = []string{cancelledMessage}
	}
	if !record.failedRun() && summary.UnchangedPresent {
		record.Unchanged = summary.UnchangedDocuments
	}
	return record
}

func recordOf(entry map[string]any) (runRecord, bool) {
	raw, ok := entry[runRecordKey]
	if !ok || raw == nil {
		return runRecord{}, false
	}
	switch typed := raw.(type) {
	case runRecord:
		return typed, typed.Status != ""
	case *runRecord:
		return *typed, typed.Status != ""
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return runRecord{}, false
	}
	var record runRecord
	if err := json.Unmarshal(encoded, &record); err != nil || record.Status == "" {
		return runRecord{}, false
	}
	return record, true
}

// ReportLabels are the nouns a report counts in.
type ReportLabels struct {
	Singular string `json:"singular"`
	Plural   string `json:"plural"`
}

// ReportTotals are a report's counts.
type ReportTotals struct {
	Indexed             uint64 `json:"indexed"`
	Skipped             uint64 `json:"skipped"`
	NotIndexed          uint64 `json:"not_indexed"`
	Failed              uint64 `json:"failed"`
	Unchanged           uint64 `json:"unchanged"`
	DependentNotIndexed uint64 `json:"dependent_not_indexed"`
	Total               uint64 `json:"total"`
}

// ReportGroup is one reason within a category. Code is the raw skip token of a
// reason the mapping table does not know: the label is generic and the token
// is the detail.
type ReportGroup struct {
	Reason  string   `json:"reason"`
	Label   string   `json:"label"`
	Code    string   `json:"code,omitempty"`
	Count   uint64   `json:"count"`
	Items   []string `json:"items"`
	Counted bool     `json:"counted"`
}

// ReportCategory is one kind of outcome.
type ReportCategory struct {
	Kind   string        `json:"kind"`
	Count  uint64        `json:"count"`
	Groups []ReportGroup `json:"groups"`
}

// Report is the SDK-shaped report the web client's normalizeIndexingReport
// reads from `report`.
type Report struct {
	Version         int              `json:"version"`
	Status          string           `json:"status"`
	ItemLabels      ReportLabels     `json:"item_labels"`
	DependentLabels ReportLabels     `json:"dependent_labels"`
	Totals          ReportTotals     `json:"totals"`
	Categories      []ReportCategory `json:"categories"`
	Errors          []string         `json:"errors"`
	ErrorsTotal     int              `json:"errors_total"`
}

// Report builds the full canonical report of a record.
func (r runRecord) Report() Report {
	counts := map[string]uint64{}
	groups := map[string][]ReportGroup{}
	index := map[string]int{}
	reasons := make([]string, 0, len(r.Skipped))
	for reason := range r.Skipped {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		count := r.Skipped[reason]
		if count == 0 {
			continue
		}
		spec, known := groupForSkipReason(reason)
		// An unknown token is keyed apart from every known group, so it can
		// never be merged into one whatever its text.
		key := spec.kind + "/" + spec.reason
		if !known {
			key = "?" + key
		}
		if at, ok := index[key]; ok {
			groups[spec.kind][at].Count += count
		} else {
			group := ReportGroup{Reason: spec.reason, Label: spec.label, Count: count, Items: []string{}, Counted: true}
			if !known {
				group.Code = reason
			}
			index[key] = len(groups[spec.kind])
			groups[spec.kind] = append(groups[spec.kind], group)
		}
		counts[spec.kind] += count
	}
	if r.Unchanged > 0 {
		// First, as the SDK lists it.
		unchanged := ReportGroup{
			Reason: "unchanged", Label: "Already indexed (unchanged)", Count: r.Unchanged, Items: []string{}, Counted: false,
		}
		groups[kindSkipped] = append([]ReportGroup{unchanged}, groups[kindSkipped]...)
	}

	categories := make([]ReportCategory, 0, len(reportKindOrder))
	total := r.Unchanged
	for _, kind := range reportKindOrder {
		count := counts[kind]
		if kind == kindIndexed {
			count = r.Indexed
		}
		total += count
		list := groups[kind]
		if list == nil {
			list = []ReportGroup{}
		}
		categories = append(categories, ReportCategory{Kind: kind, Count: count, Groups: list})
	}

	errors := append([]string{}, r.Errors...)
	if r.FailedChunks > 0 {
		errors = append(errors, fmt.Sprintf("%d chunk(s) could not be embedded or stored", r.FailedChunks))
	}
	return Report{
		Version:         ReportVersion,
		Status:          r.Status,
		ItemLabels:      ReportLabels{Singular: "document", Plural: "documents"},
		DependentLabels: ReportLabels{Singular: "attachment", Plural: "attachments"},
		Totals: ReportTotals{
			Indexed: r.Indexed, Skipped: counts[kindSkipped], NotIndexed: counts[kindNotIndexed],
			Failed: counts[kindFailed], Unchanged: r.Unchanged, Total: total,
		},
		Categories:  categories,
		Errors:      errors,
		ErrorsTotal: len(errors),
	}
}

// Slim is the report without what the web client defaults and without the
// empty categories: the form history entries take in a list response, which
// repeats one per run. normalizeIndexingReport renders it the same.
func (r Report) Slim() map[string]any {
	categories := make([]map[string]any, 0, len(r.Categories))
	for _, category := range r.Categories {
		if category.Count == 0 && len(category.Groups) == 0 {
			continue
		}
		groups := make([]map[string]any, 0, len(category.Groups))
		for _, group := range category.Groups {
			slim := map[string]any{"reason": group.Reason, "label": group.Label, "count": group.Count, "counted": group.Counted}
			if group.Code != "" {
				slim["code"] = group.Code
			}
			groups = append(groups, slim)
		}
		categories = append(categories, map[string]any{"kind": category.Kind, "count": category.Count, "groups": groups})
	}
	return map[string]any{
		"version": r.Version, "status": r.Status, "totals": r.Totals, "categories": categories,
		"errors": r.Errors, "errors_total": r.ErrorsTotal,
	}
}

// expandEntry is a history entry as served: its stored record (already
// decoded by the caller) becomes a report, the full canonical one when full,
// else the slim one. The stored entry is not changed.
func expandEntry(entry map[string]any, record runRecord, decoded, full bool) map[string]any {
	if !decoded {
		if _, has := entry[runRecordKey]; !has {
			return entry
		}
	}
	out := copyObject(entry)
	delete(out, runRecordKey)
	if decoded {
		report := record.Report()
		if full {
			out["report"] = report
		} else {
			out["report"] = report.Slim()
		}
		out["total"] = report.Totals.Total
	}
	return out
}
