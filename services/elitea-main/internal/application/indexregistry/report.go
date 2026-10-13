package indexregistry

import (
	"fmt"
	"sort"
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
}

// groupForSkipReason is the group of a skip reason. A reason this table does
// not know (a newer worker) is never dropped: it lands in not_indexed under its
// own token.
func groupForSkipReason(reason string) reportGroupSpec {
	if spec, ok := skipReasonGroups[reason]; ok {
		return spec
	}
	return reportGroupSpec{kindNotIndexed, reason, "Not indexed (" + reason + ")"}
}

// terminalState reports whether a run has ended and so has a report.
func terminalState(state State) bool {
	switch state {
	case StateCompleted, StatePartlyIndexed, StateFailed, StateCancelled:
		return true
	}
	return false
}

// runReport builds the SDK-shaped report of the row's last run, or nil when
// there is none to build: the run is still going, or it ended without ever
// reporting (a row the Python path owns), where an invented all-zero report
// would read as a clean run.
//
// `indexed` is the documents this run wrote; documents the index holds that
// the run did not touch are the SDK's "unchanged" (total_documents minus
// indexed), reported beside the categories and never inside `skipped`.
func runReport(row Row) map[string]any {
	failedRun := row.State == StateFailed || row.State == StateCancelled
	if !terminalState(row.State) || (!row.Stamped() && !failedRun) {
		return nil
	}

	counts := map[string]int64{}
	groups := map[string][]any{}
	merged := map[string]map[string]any{}
	reasons := make([]string, 0, len(row.Skipped))
	for reason := range row.Skipped {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		count := int64(row.Skipped[reason])
		if count == 0 {
			continue
		}
		spec := groupForSkipReason(reason)
		key := spec.kind + "/" + spec.reason
		if group, ok := merged[key]; ok {
			group["count"] = group["count"].(int64) + count
		} else {
			group = map[string]any{
				"reason": spec.reason,
				"label":  spec.label,
				"count":  count,
				"items":  []any{},
			}
			merged[key] = group
			groups[spec.kind] = append(groups[spec.kind], group)
		}
		counts[spec.kind] += count
	}

	unchanged := int64(0)
	if row.Stamped() && row.TotalDocuments > row.Indexed {
		unchanged = row.TotalDocuments - row.Indexed
	}
	if unchanged > 0 {
		groups[kindSkipped] = append(groups[kindSkipped], map[string]any{
			"reason":  "unchanged",
			"label":   "Already indexed (unchanged)",
			"count":   unchanged,
			"items":   []any{},
			"counted": false,
		})
	}

	categories := make([]any, 0, len(reportKindOrder))
	total := unchanged
	for _, kind := range reportKindOrder {
		count := counts[kind]
		if kind == kindIndexed {
			count = row.Indexed
		}
		total += count
		list := groups[kind]
		if list == nil {
			list = []any{}
		}
		categories = append(categories, map[string]any{"kind": kind, "count": count, "groups": list})
	}

	errors := []any{}
	if row.Error != nil && *row.Error != "" {
		errors = append(errors, *row.Error)
	}
	if row.Failed > 0 {
		errors = append(errors, fmt.Sprintf("%d chunk(s) could not be embedded or stored", row.Failed))
	}

	status := "ok"
	switch row.State {
	case StatePartlyIndexed:
		status = "partly_indexed"
	case StateFailed, StateCancelled:
		status = "error"
	}

	return map[string]any{
		"version":          ReportVersion,
		"status":           status,
		"item_labels":      map[string]any{"singular": "document", "plural": "documents"},
		"dependent_labels": map[string]any{"singular": "attachment", "plural": "attachments"},
		"totals": map[string]any{
			"indexed":               row.Indexed,
			"skipped":               counts[kindSkipped],
			"not_indexed":           counts[kindNotIndexed],
			"failed":                counts[kindFailed],
			"unchanged":             unchanged,
			"dependent_not_indexed": 0,
			"total":                 total,
		},
		"categories":   categories,
		"errors":       errors,
		"errors_total": len(errors),
	}
}
