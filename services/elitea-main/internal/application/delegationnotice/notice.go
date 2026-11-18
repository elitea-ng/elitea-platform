// Package delegationnotice is the chat notice of a refused or narrowed
// delegation (ADR-0044 Amendment 1, A1.2 rule 5; delegation epic 2156 story
// S5).
//
// The Rust worker emits a `delegation_refused` or `delegation_narrowed` node
// event next to the refusal the model reads. Main keeps the notice on the
// answer (`meta.delegation_notices_v1`, bounded and closed, so a reload shows
// it) and serves it with what the viewer may do about it: `can_review` (the
// viewer may edit agents in the project, so may open the grant review) and,
// for a viewer who may not, the names of the people to ask. Both are derived
// per viewer on the way out and never stored.
//
// The notice carries names and ids only. A credential value or a tool
// argument never reaches it: every field is rebuilt from a closed set.
package delegationnotice

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	// EventRefused is the node event type of a refused delegation.
	EventRefused = "delegation_refused"
	// EventNarrowed is the node event type of a narrowed delegation.
	EventNarrowed = "delegation_narrowed"
	// FrameKey is the `response_metadata` member that carries the notice.
	FrameKey = "delegation_v1"
	// MetaKey is the answer's meta key the notices are kept under.
	MetaKey = "delegation_notices_v1"
	// MaxNotices is how many notices an answer keeps.
	MaxNotices = 16
	// EditPermission is the project permission that edits an agent: who may
	// open the grant review.
	EditPermission = "models.applications.application.update"
	// MaxEditors is how many names a notice offers a viewer who may not review.
	MaxEditors = 3

	// UnnamedAgent stands in for a callee whose name is blank.
	UnnamedAgent = "an agent"

	maxMissing   = 16
	maxIDBytes   = 128
	maxNameRunes = 120
)

// FrameMarker is the frame key as it appears in a frame's JSON, for the cheap
// check every accepted frame passes.
var FrameMarker = []byte(`"` + FrameKey + `"`)

var (
	kinds   = map[string]bool{"refused": true, "narrowed": true}
	reasons = map[string]bool{"no_grant": true, "stale": true, "principal": true, "depth": true}
	codes   = map[string]bool{"DELEGATION_GRANT_MISSING": true, "DELEGATION_DEPTH_EXCEEDED": true}
	grants  = map[string]bool{"toolkit": true, "tool": true, "credential": true, "data_class": true, "egress_host": true}
	edges   = map[string]bool{"agent_tool": true, "pipeline_child": true, "sub_agent": true, "pipeline_node": true}
)

// Item is one grant a notice names.
type Item struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Toolkit string `json:"toolkit,omitempty"`
}

// Edge is the composition edge the delegation crossed.
type Edge struct {
	CallerID   int64  `json:"caller_id,omitempty"`
	CallerName string `json:"caller_name,omitempty"`
	CalleeID   int64  `json:"callee_id,omitempty"`
	CalleeName string `json:"callee_name"`
	Kind       string `json:"kind"`
}

// Notice is one refused or narrowed delegation, as the answer keeps it.
type Notice struct {
	// ID is the tool call the delegation answered: the notice sits at its place.
	ID string `json:"id"`
	// Kind is `refused` or `narrowed`.
	Kind string `json:"kind"`
	// Reason is `no_grant`, `stale`, `principal` or `depth`.
	Reason       string `json:"reason"`
	Code         string `json:"code,omitempty"`
	Edge         Edge   `json:"edge"`
	Missing      []Item `json:"missing"`
	MissingTotal int    `json:"missing_total,omitempty"`
	GrantID      int64  `json:"grant_id,omitempty"`
	Depth        int    `json:"depth,omitempty"`
	MaxDepth     int    `json:"max_depth,omitempty"`
}

// Sanitize rebuilds a notice from the worker's value into the closed, bounded
// shape. It answers false for anything that is not one, so a malformed notice
// is ignored and never fails the turn.
func Sanitize(raw json.RawMessage) (Notice, bool) {
	var in struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		Reason       string `json:"reason"`
		Code         string `json:"code"`
		Edge         Edge   `json:"edge"`
		Missing      []Item `json:"missing"`
		MissingTotal int    `json:"missing_total"`
		GrantID      int64  `json:"grant_id"`
		Depth        int    `json:"depth"`
		MaxDepth     int    `json:"max_depth"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return Notice{}, false
	}
	if in.ID == "" || len(in.ID) > maxIDBytes || !kinds[in.Kind] || !reasons[in.Reason] ||
		!edges[in.Edge.Kind] || (in.Code != "" && !codes[in.Code]) {
		return Notice{}, false
	}
	// A refusal is never reviewable by a grant when the principal or the depth
	// caused it, and a narrowing is always drift: the reason must match the kind.
	if (in.Kind == "narrowed") != (in.Reason == "stale") {
		return Notice{}, false
	}
	out := Notice{
		ID: in.ID, Kind: in.Kind, Reason: in.Reason, Code: in.Code,
		Edge: Edge{
			CallerID: positive(in.Edge.CallerID), CallerName: clip(in.Edge.CallerName),
			CalleeID: positive(in.Edge.CalleeID), CalleeName: clip(in.Edge.CalleeName),
			Kind: in.Edge.Kind,
		},
		Missing:      []Item{},
		MissingTotal: max(0, min(in.MissingTotal, 1<<20)),
		GrantID:      positive(in.GrantID),
		Depth:        max(0, min(in.Depth, 1<<10)),
		MaxDepth:     max(0, min(in.MaxDepth, 1<<10)),
	}
	// A name the agent does not have (blank, or only control characters) is
	// still a refusal the person must see.
	if out.Edge.CalleeName == "" {
		out.Edge.CalleeName = UnnamedAgent
	}
	for _, item := range in.Missing {
		if len(out.Missing) == maxMissing {
			break
		}
		name := clip(item.Name)
		if !grants[item.Kind] || name == "" {
			continue
		}
		out.Missing = append(out.Missing, Item{Kind: item.Kind, Name: name, Toolkit: clip(item.Toolkit)})
	}
	return out, true
}

// Merge keeps notices in arrival order, one per tool call, at most MaxNotices.
// A notice for a call already kept replaces it.
func Merge(existing []Notice, incoming Notice) []Notice {
	for i := range existing {
		if existing[i].ID == incoming.ID {
			out := append([]Notice(nil), existing...)
			out[i] = incoming
			return out
		}
	}
	if len(existing) >= MaxNotices {
		return existing
	}
	return append(append([]Notice(nil), existing...), incoming)
}

// Decode reads an answer's stored notices; anything unreadable reads as none.
func Decode(raw []byte) []Notice {
	var stored []json.RawMessage
	if json.Unmarshal(raw, &stored) != nil {
		return nil
	}
	var out []Notice
	for _, entry := range stored {
		if notice, ok := Sanitize(entry); ok {
			out = Merge(out, notice)
		}
	}
	return out
}

// Review is what one viewer may do about a delegation notice.
type Review struct {
	// CanReview is whether the viewer may open the grant review.
	CanReview bool
	// Editors are the people to ask when the viewer may not, at most MaxEditors.
	Editors []string
}

// Reviewer answers a viewer's Review of the delegations of one agent.
type Reviewer interface {
	// DelegationReview answers for the user in the project and the calling
	// agent (0 when the caller has no saved identity).
	DelegationReview(ctx context.Context, projectID, userID, callerID int64) (Review, error)
}

// Stamp returns the notice as a viewer is served it: `can_review`, and the
// editors' names when the viewer may not review. A principal or depth refusal
// is never reviewable (an editor cannot fix it), so it carries neither.
func Stamp(notice map[string]any, review Review) {
	switch notice["reason"] {
	case "principal", "depth":
		notice["can_review"] = false
		delete(notice, "editors")
		return
	}
	notice["can_review"] = review.CanReview
	if review.CanReview || len(review.Editors) == 0 {
		delete(notice, "editors")
		return
	}
	names := review.Editors
	if len(names) > MaxEditors {
		names = names[:MaxEditors]
	}
	notice["editors"] = names
}

// CallerOf is the calling agent of a notice in a decoded frame or meta value.
func CallerOf(notice map[string]any) int64 {
	edge, _ := notice["edge"].(map[string]any)
	switch id := edge["caller_id"].(type) {
	case float64:
		return int64(id)
	case json.Number:
		parsed, _ := id.Int64()
		return parsed
	}
	return 0
}

// StampFrame stamps the notice of one served node event. It answers the frame
// unchanged when it carries none (or cannot be read), so the stream never
// fails on a display decoration.
func StampFrame(ctx context.Context, data []byte, projectID, userID int64, reviewer Reviewer) []byte {
	if reviewer == nil || !bytes.Contains(data, FrameMarker) {
		return data
	}
	var frame map[string]json.RawMessage
	if json.Unmarshal(data, &frame) != nil {
		return data
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(frame["response_metadata"], &metadata) != nil {
		return data
	}
	var notice map[string]any
	decoder := json.NewDecoder(bytes.NewReader(metadata[FrameKey]))
	decoder.UseNumber()
	if decoder.Decode(&notice) != nil || notice == nil {
		return data
	}
	review, err := reviewer.DelegationReview(ctx, projectID, userID, CallerOf(notice))
	if err != nil {
		return data
	}
	Stamp(notice, review)
	encoded, err := json.Marshal(notice)
	if err != nil {
		return data
	}
	metadata[FrameKey] = encoded
	if frame["response_metadata"], err = json.Marshal(metadata); err != nil {
		return data
	}
	out, err := json.Marshal(frame)
	if err != nil {
		return data
	}
	return out
}

// StampMeta returns the served answer's meta with its notices stamped. The
// map it was given is never modified: it may be shared with a cache, and
// another viewer's stamp must not leak into it. It returns meta itself when
// there is nothing to stamp.
func StampMeta(ctx context.Context, meta map[string]any, projectID, userID int64, reviewer Reviewer) map[string]any {
	if reviewer == nil {
		return meta
	}
	stored, ok := meta[MetaKey].([]any)
	if !ok || len(stored) == 0 {
		return meta
	}
	stamped := make([]any, 0, len(stored))
	for _, entry := range stored {
		notice, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		copied := make(map[string]any, len(notice)+2)
		for key, value := range notice {
			copied[key] = value
		}
		review, err := reviewer.DelegationReview(ctx, projectID, userID, CallerOf(copied))
		if err == nil {
			Stamp(copied, review)
		}
		stamped = append(stamped, copied)
	}
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		out[key] = value
	}
	out[MetaKey] = stamped
	return out
}

func positive(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// clip bounds a display string: valid UTF-8, no control characters, trimmed.
func clip(text string) string {
	if !utf8.ValidString(text) {
		return ""
	}
	text = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > maxNameRunes {
		return string([]rune(text)[:maxNameRunes])
	}
	return text
}
