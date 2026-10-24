package conversations

// Rewind and the fork's request options (issue 1234).
//
// A rewind is a fork (`mode: "rewind"`) that also records what the turns
// after the fork point tried: the branch summary, a `context_summary` v1
// record (libs/rust/agent-runtime/src/context_summary.rs, at most 32 KiB).
// It is stored on the new conversation as `meta.forked_from.branch_summary`,
// so a desktop that rebuilds its session from the server (a cold seed) adds
// it after the fork point too. Both conversations stay linear: the source is
// not changed, and the client lists it as the earlier attempt.
//
// The client may send the summary. When it sends none, the server builds one
// from the text of the abandoned turns (buildBranchSummary): their questions
// and answers, cut to the record's bounds. No model is called here.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The fork modes (`mode` on the request and on `forked_from`).
const (
	ForkModeFork   = "fork"
	ForkModeRewind = "rewind"
)

// Bounds of a `context_summary` v1 record (context_summary.rs).
const (
	maxBranchSummaryBytes = 32 * 1024
	maxSummaryItems       = 24
	maxSummaryTextBytes   = 2048
)

// ErrForkIDReused: the request's `fork_id` already made a fork of another
// source, fork point or mode. Answered 409 `fork_id_conflict`.
var ErrForkIDReused = errors.New("conversations: fork_id was used for another fork")

// ErrForkTooLarge: the fork would copy more than the bound of message groups.
// Answered 413 `fork_too_large`.
var ErrForkTooLarge = errors.New("conversations: the fork copies too many messages")

// ErrThreadExecutorIsCloud: the source is a Local work thread the cloud
// executor owns (ADR-0034). Answered 409 `thread_executor_is_cloud`.
var ErrThreadExecutorIsCloud = errors.New("conversations: the cloud executor owns the source thread")

// AbandonedTurn is one message group of the source after the fork point that
// a rewind leaves behind: a question (Question) or an answer, and its text.
type AbandonedTurn struct {
	Question bool
	Text     string
}

type summaryReference struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type summaryWork struct {
	Result       string   `json:"result"`
	EvidenceRefs []string `json:"evidence_refs"`
}

// branchSummary is the `context_summary` v1 record, field for field.
type branchSummary struct {
	Version          int                `json:"version"`
	Objective        string             `json:"objective"`
	Constraints      []string           `json:"constraints"`
	Decisions        []string           `json:"decisions"`
	KeyFacts         []string           `json:"key_facts"`
	CompletedWork    []summaryWork      `json:"completed_work"`
	OpenWork         []string           `json:"open_work"`
	NextSteps        []string           `json:"next_steps"`
	UnresolvedIssues []string           `json:"unresolved_issues"`
	References       []summaryReference `json:"references"`
}

// forkOptions reads `mode`, `branch_summary` and `fork_id` into request.
// A rewind names its fork point; a branch summary is a rewind's only.
func forkOptions(body map[string]any, request *ForkRequest) error {
	request.Mode = ForkModeFork
	if value, present := body["mode"]; present && value != nil {
		mode, _ := value.(string)
		if mode != ForkModeFork && mode != ForkModeRewind {
			return apierr.BadRequest(`mode must be "fork" or "rewind"`)
		}
		request.Mode = mode
	}
	if request.Mode == ForkModeRewind && request.MessageID == "" {
		return apierr.BadRequest("a rewind needs message_id: the answer to rewind to")
	}
	if value, present := body["branch_summary"]; present && value != nil {
		if request.Mode != ForkModeRewind {
			return apierr.BadRequest(`branch_summary is allowed only with mode "rewind"`)
		}
		summary, err := validBranchSummary(value)
		if err != nil {
			return err
		}
		request.BranchSummary = summary
	}
	if value, present := body["fork_id"]; present && value != nil {
		text, _ := value.(string)
		parsed, err := uuid.Parse(text)
		if err != nil {
			return apierr.BadRequest("fork_id must be a UUID")
		}
		request.ForkID = parsed.String()
	}
	return nil
}

// validBranchSummary checks a client's branch summary against the v1 record
// (its keys, its version and its bounds) and answers it as stored.
func validBranchSummary(value any) (map[string]any, error) {
	invalid := apierr.BadRequest("branch_summary must be a context_summary v1 record of at most 32 KiB")
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxBranchSummaryBytes {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var summary branchSummary
	if err := decoder.Decode(&summary); err != nil || summary.Version != 1 || !summaryBounded(summary) {
		return nil, invalid
	}
	return summaryMap(summary), nil
}

func summaryBounded(s branchSummary) bool {
	text := func(value string) bool { return len(value) <= maxSummaryTextBytes && utf8.ValidString(value) }
	list := func(values []string) bool {
		if len(values) > maxSummaryItems {
			return false
		}
		for _, value := range values {
			if !text(value) {
				return false
			}
		}
		return true
	}
	if !text(s.Objective) || !list(s.Constraints) || !list(s.Decisions) || !list(s.KeyFacts) ||
		!list(s.OpenWork) || !list(s.NextSteps) || !list(s.UnresolvedIssues) ||
		len(s.CompletedWork) > maxSummaryItems || len(s.References) > maxSummaryItems {
		return false
	}
	for _, work := range s.CompletedWork {
		if !text(work.Result) || !list(work.EvidenceRefs) {
			return false
		}
	}
	for _, reference := range s.References {
		if !text(reference.Label) || !text(reference.Value) {
			return false
		}
	}
	return true
}

// summaryMap is the record as `meta` stores it, every array present.
func summaryMap(s branchSummary) map[string]any {
	list := func(values []string) []any {
		out := make([]any, 0, len(values))
		for _, value := range values {
			out = append(out, value)
		}
		return out
	}
	work := make([]any, 0, len(s.CompletedWork))
	for _, item := range s.CompletedWork {
		work = append(work, map[string]any{"result": item.Result, "evidence_refs": list(item.EvidenceRefs)})
	}
	references := make([]any, 0, len(s.References))
	for _, item := range s.References {
		references = append(references, map[string]any{"label": item.Label, "value": item.Value})
	}
	return map[string]any{
		"version":           1,
		"objective":         s.Objective,
		"constraints":       list(s.Constraints),
		"decisions":         list(s.Decisions),
		"key_facts":         list(s.KeyFacts),
		"completed_work":    work,
		"open_work":         list(s.OpenWork),
		"next_steps":        list(s.NextSteps),
		"unresolved_issues": list(s.UnresolvedIssues),
		"references":        references,
	}
}

// BuildBranchSummary is the branch summary the server writes for a rewind
// whose request carried none: the abandoned turns, oldest first, as
// `key_facts` ("Asked: …" and "Answered: …", each cut), and the first
// question as the objective. Nothing in it is claimed as done: the outcome
// of those turns is what the person rewound away from.
func BuildBranchSummary(turns []AbandonedTurn) map[string]any {
	summary := branchSummary{
		Version:   1,
		Objective: "Continue the conversation from before the abandoned attempt.",
		UnresolvedIssues: []string{
			"The person rewound the conversation to before these turns and abandoned them. " +
				"Their results do not stand, and files they changed may have been restored or kept.",
		},
	}
	// The record's 32 KiB, less the fixed part, with room for JSON escapes
	// (a string may double): each fact is cut to a share of what is left.
	budget := maxBranchSummaryBytes/2 - 1024
	asked := false
	for _, turn := range turns {
		text := strings.Join(strings.Fields(strings.ToValidUTF8(turn.Text, " ")), " ")
		if text == "" {
			continue
		}
		if turn.Question && !asked {
			asked = true
			summary.Objective = "The abandoned attempt was asked: " + clipText(text, maxSummaryTextBytes-64)
		}
		if len(summary.KeyFacts) >= maxSummaryItems || budget <= 64 {
			break
		}
		prefix := "Answered: "
		if turn.Question {
			prefix = "Asked: "
		}
		fact := prefix + clipText(text, min(maxSummaryTextBytes/2, budget)-len(prefix))
		budget -= len(fact)
		summary.KeyFacts = append(summary.KeyFacts, fact)
	}
	return summaryMap(summary)
}

// clipText cuts text to at most limit bytes on a character boundary, with
// an ellipsis when it was cut.
func clipText(text string, limit int) string {
	if limit <= 3 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := limit - 3
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

// forkMarkerOptions reads a stored marker's `mode` (absent on a fork made
// before rewinds existed: a fork) and `cut_message_uuid`.
func forkMarkerOptions(value map[string]any, out map[string]any) {
	mode := ForkModeFork
	if text, ok := value["mode"].(string); ok && text == ForkModeRewind {
		mode = ForkModeRewind
	}
	out["mode"] = mode
	if text, ok := value["cut_message_uuid"].(string); ok && validUUID(text) {
		out["cut_message_uuid"] = text
	}
}

// annotateBranchSummary adds a rewind's stored branch summary to a
// getConversation answer's `forked_from`, the read a desktop seeds its
// session from. A stored value that is not a valid record is left out.
func annotateBranchSummary(meta map[string]any, ff map[string]any) {
	value, ok := meta[ForkedFromMetaKey].(map[string]any)
	if !ok || ff["mode"] != ForkModeRewind || value["branch_summary"] == nil {
		return
	}
	if summary, err := validBranchSummary(value["branch_summary"]); err == nil {
		ff["branch_summary"] = summary
	}
}
