package repos

// The cloud sub-agent lane (ADR-0050 decision 4, #1823). The Rust worker
// streams each `spawn_agent` child's lifecycle as `agent_subagent` node
// events on the run's own output stream: its start (display name, avatar
// key, label, depth and parent agent id, #1872), its own tool calls and
// results (summarised), its text and its end. Clients fold them into a lane
// under the `spawn_agent` row while the run streams. Main folds the same
// entries onto the answer's lane, in the transaction that accepts the
// frame, so a reloaded transcript shows the settled lane: the spawn record
// of #1614 (`elitea_runtime.ephemeral_agents`) keeps no tool calls or text.
//
// Storage (issue 1923, #1823 follow-up): each lane is its own row in
// `chat_message_subagent_lanes`, keyed by (message_group_id, agent_id).
// Before this the answer's WHOLE `meta.subagents_v1` array was re-encoded
// and the whole `meta` TOASTed value rewritten for every accepted frame,
// which cost O(n^2) bytes per turn at the lane bounds (measured: 111 MB of
// WAL for a 12-child, 48-item turn). Updating one lane's own row instead
// measured 9.8 MB of WAL for the same turn. `decodeAnswerSubAgentLanes`
// folds a message group's rows back into `[]subAgentLane` on read, in
// ordinal order, so every reader (the chat page, the details route, the
// inspector) sees the same `SubAgentLane[]` shape client contract 1.48 and
// 1.50 (#1872 nesting) describe, whichever storage wrote it.
//
// The lane is display data. Every value is rebuilt into a closed, bounded
// shape; an entry that does not fit (or a frame without one) is ignored
// rather than refusing the frame, so a malformed lane entry never fails the
// turn. A frame bound to another answer is still refused, as every
// projection of the run's response binding refuses it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

const (
	subAgentLaneEventType   = "agent_subagent"
	subAgentLaneMetadataKey = "subagent_v1"
	// subAgentLanesMetaKey is the answer's meta key the lanes are kept under.
	subAgentLanesMetaKey = "subagents_v1"
	// The worker's bounds (services/elitea-worker-rust/src/agents/
	// subagent_lane.rs): 12 children per turn, 48 tool and text entries per
	// child run. A continued child runs again, so its items are bounded
	// here as a whole.
	maxSubAgentLanes      = 24
	maxSubAgentLaneItems  = 96
	maxSubAgentIDBytes    = 128
	maxSubAgentNameRunes  = 120
	maxSubAgentShortRunes = 240
	maxSubAgentTextRunes  = 600
	maxSubAgentResultRune = 2000
)

// subAgentLaneEventMarker is the frame type as it appears in a frame's JSON.
var subAgentLaneEventMarker = []byte(`"` + subAgentLaneEventType + `"`)

var subAgentLaneStatuses = map[string]bool{
	"running": true, "completed": true, "failed": true, "budget_exhausted": true, "cancelled": true,
}

// subAgentLaneItem is one tool row or text of a lane.
type subAgentLaneItem struct {
	Type        string `json:"type"`
	CallID      string `json:"call_id,omitempty"`
	Tool        string `json:"tool,omitempty"`
	ArgsSummary string `json:"args_summary,omitempty"`
	OK          *bool  `json:"ok,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Text        string `json:"text,omitempty"`
}

// subAgentLane is one child as `chat_message_subagent_lanes.lane` and the
// wire (`SubAgentLane`) keep it. Depth and ParentAgentID (#1872) travel in
// this JSON rather than as their own columns: they are read-side nesting
// data, never queried on their own.
type subAgentLane struct {
	AgentID      string             `json:"agent_id"`
	ParentCallID string             `json:"parent_call_id"`
	Ordinal      int                `json:"ordinal"`
	// Depth is 1 for a child of the run's agent, 2 for a child of a child
	// (#1824). 0 (a lane written before #1872) reads as 1.
	Depth int `json:"depth,omitempty"`
	// ParentAgentID is the sub-agent that started this one; empty at depth
	// 1. #1872: `decodeAnswerSubAgentLanes` nests a depth-2 lane under the
	// lane of the agent_id this names.
	ParentAgentID string             `json:"parent_agent_id,omitempty"`
	Label         string             `json:"label,omitempty"`
	DisplayName   string             `json:"display_name,omitempty"`
	Avatar        string             `json:"avatar,omitempty"`
	Status        string             `json:"status"`
	Items         []subAgentLaneItem `json:"items"`
	Steps         *int64             `json:"steps,omitempty"`
	InputTokens   *int64             `json:"input_tokens,omitempty"`
	OutputTokens  *int64             `json:"output_tokens,omitempty"`
	Result        string             `json:"result,omitempty"`
	Error         string             `json:"error,omitempty"`
	// Children (#1872): the depth-2 lanes `decodeAnswerSubAgentLanes`
	// nested under this one on read. Never stored: each child is its own
	// row, keyed by its own agent_id; this is folded in only for the wire.
	Children []subAgentLane `json:"children,omitempty"`
}

// subAgentLaneEntry is one `subagent_v1` entry as the worker sends it.
type subAgentLaneEntry struct {
	Kind          string `json:"kind"`
	AgentID       string `json:"agent_id"`
	ParentCallID  string `json:"parent_call_id"`
	Ordinal       int    `json:"ordinal"`
	Depth         int    `json:"depth"`
	ParentAgentID string `json:"parent_agent_id"`
	Label         string `json:"label"`
	DisplayName   string `json:"display_name"`
	Avatar        string `json:"avatar"`
	CallID        string `json:"call_id"`
	Tool          string `json:"tool"`
	ArgsSummary   string `json:"args_summary"`
	OK            *bool  `json:"ok"`
	Summary       string `json:"summary"`
	Text          string `json:"text"`
	Status        string `json:"status"`
	Steps         *int64 `json:"steps"`
	InputTokens   *int64 `json:"input_tokens"`
	OutputTokens  *int64 `json:"output_tokens"`
	Result        string `json:"result"`
	Error         string `json:"error"`
}

// laneText is `value` without control characters other than newline and
// tab, cut to `max` runes.
func laneText(value string, max int) string {
	if !utf8.ValidString(value) {
		return ""
	}
	var out strings.Builder
	count := 0
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			continue
		}
		if count == max {
			break
		}
		out.WriteRune(r)
		count++
	}
	return out.String()
}

func laneID(value string) (string, bool) {
	if value == "" || len(value) > maxSubAgentIDBytes || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	return value, true
}

func nonNegative(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	return value
}

// mergeSubAgentLanes folds one entry into the lanes; unchanged (false) when
// the entry does not apply.
func mergeSubAgentLanes(lanes []subAgentLane, raw json.RawMessage) ([]subAgentLane, bool) {
	var entry subAgentLaneEntry
	if json.Unmarshal(raw, &entry) != nil {
		return lanes, false
	}
	agentID, ok := laneID(entry.AgentID)
	if !ok {
		return lanes, false
	}
	index := -1
	for i := range lanes {
		if lanes[i].AgentID == agentID {
			index = i
			break
		}
	}
	if entry.Kind == "started" {
		parent, ok := laneID(entry.ParentCallID)
		if !ok || index != -1 || len(lanes) >= maxSubAgentLanes || entry.Ordinal < 1 || entry.Ordinal > maxSubAgentLanes {
			return lanes, false
		}
		// Depth is 1 or 2 (#1824); 0 (an older worker) reads as 1.
		// ParentAgentID is the sub-agent that started this one, required at
		// depth 2, and ignored (never trusted at depth 1) otherwise — #1872
		// nests only what a depth-2 lane itself names.
		depth := entry.Depth
		if depth < 1 {
			depth = 1
		}
		if depth > 2 {
			depth = 2
		}
		var parentAgentID string
		if depth == 2 {
			id, ok := laneID(entry.ParentAgentID)
			if !ok {
				return lanes, false
			}
			parentAgentID = id
		}
		return append(lanes, subAgentLane{
			AgentID: agentID, ParentCallID: parent, Ordinal: entry.Ordinal,
			Depth: depth, ParentAgentID: parentAgentID,
			Label:       laneText(entry.Label, maxSubAgentNameRunes),
			DisplayName: laneText(entry.DisplayName, maxSubAgentNameRunes),
			Avatar:      laneText(entry.Avatar, maxSubAgentNameRunes),
			Status:      "running", Items: []subAgentLaneItem{},
		}), true
	}
	if index == -1 {
		return lanes, false
	}
	next := append([]subAgentLane(nil), lanes...)
	lane := next[index]
	lane.Items = append([]subAgentLaneItem(nil), lane.Items...)
	switch entry.Kind {
	case "resumed":
		lane.Status = "running"
		lane.Result, lane.Error = "", ""
	case "tool_start":
		callID, ok := laneID(entry.CallID)
		if !ok || len(lane.Items) >= maxSubAgentLaneItems {
			return lanes, false
		}
		for _, item := range lane.Items {
			if item.Type == "tool" && item.CallID == callID {
				return lanes, false
			}
		}
		lane.Items = append(lane.Items, subAgentLaneItem{
			Type: "tool", CallID: callID,
			Tool:        laneText(entry.Tool, maxSubAgentNameRunes),
			ArgsSummary: laneText(entry.ArgsSummary, maxSubAgentShortRunes),
		})
	case "tool_end":
		callID, ok := laneID(entry.CallID)
		if !ok || entry.OK == nil {
			return lanes, false
		}
		found := false
		for i := range lane.Items {
			if lane.Items[i].Type == "tool" && lane.Items[i].CallID == callID {
				okValue := *entry.OK
				lane.Items[i].OK = &okValue
				lane.Items[i].Summary = laneText(entry.Summary, maxSubAgentShortRunes)
				found = true
			}
		}
		if !found {
			return lanes, false
		}
	case "text":
		text := laneText(entry.Text, maxSubAgentTextRunes)
		if strings.TrimSpace(text) == "" || len(lane.Items) >= maxSubAgentLaneItems {
			return lanes, false
		}
		lane.Items = append(lane.Items, subAgentLaneItem{Type: "text", Text: text})
	case "finished":
		if !subAgentLaneStatuses[entry.Status] {
			return lanes, false
		}
		lane.Status = entry.Status
		if steps := nonNegative(entry.Steps); steps != nil {
			lane.Steps = steps
		}
		if tokens := nonNegative(entry.InputTokens); tokens != nil {
			lane.InputTokens = tokens
		}
		if tokens := nonNegative(entry.OutputTokens); tokens != nil {
			lane.OutputTokens = tokens
		}
		lane.Result = laneText(entry.Result, maxSubAgentResultRune)
		lane.Error = laneText(entry.Error, maxSubAgentNameRunes)
	default:
		return lanes, false
	}
	next[index] = lane
	return next, true
}

// decodeSubAgentLanes reads one lane, or a legacy whole-array value, as a
// lane list; a value that decodes as neither reads as none.
func decodeSubAgentLanes(raw []byte) []subAgentLane {
	if len(raw) == 0 {
		return nil
	}
	var lanes []subAgentLane
	if json.Unmarshal(raw, &lanes) == nil {
		return lanes
	}
	var one subAgentLane
	if json.Unmarshal(raw, &one) == nil && one.AgentID != "" {
		return []subAgentLane{one}
	}
	return nil
}

// decodeAnswerSubAgentLanes folds one answer's `chat_message_subagent_lanes`
// rows (already in ordinal order) into the `[]subAgentLane` the wire reads
// as `metadata.subagents_v1` (client contract 1.48, nesting 1.50): a
// depth-2 lane is appended to its parent's `Children` rather than kept at
// the top level, so `[]subAgentLane` is always the run's top-level
// (depth-1) children, `#1872`. A depth-2 lane whose parent row did not
// settle first (the worker always starts the parent before a grandchild;
// this is belt and braces against a malformed or partial write) is kept at
// the top level rather than dropped, so a lane that arrived is never lost.
func decodeAnswerSubAgentLanes(rows [][]byte) []subAgentLane {
	byAgentID := make(map[string]*subAgentLane, len(rows))
	var order []string
	for _, raw := range rows {
		var lane subAgentLane
		if json.Unmarshal(raw, &lane) != nil || lane.AgentID == "" {
			continue
		}
		lane.Children = nil
		if lane.Depth < 1 {
			lane.Depth = 1
		}
		byAgentID[lane.AgentID] = &lane
		order = append(order, lane.AgentID)
	}
	var topOrder []string
	for _, agentID := range order {
		lane := byAgentID[agentID]
		if lane.Depth == 2 && lane.ParentAgentID != "" && lane.ParentAgentID != agentID {
			if parent, ok := byAgentID[lane.ParentAgentID]; ok {
				parent.Children = append(parent.Children, *lane)
				continue
			}
		}
		topOrder = append(topOrder, agentID)
	}
	out := make([]subAgentLane, 0, len(topOrder))
	for _, agentID := range topOrder {
		out = append(out, *byAgentID[agentID])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	for i := range out {
		sort.SliceStable(out[i].Children, func(a, b int) bool {
			return out[i].Children[a].Ordinal < out[i].Children[b].Ordinal
		})
	}
	return out
}

// projectSubAgentLane folds one accepted `agent_subagent` frame onto its
// answer's `meta.subagents_v1`. A frame of another type is not its
// business.
func projectSubAgentLane(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame) error {
	// Every accepted agent frame passes here: only one that names the type
	// is decoded.
	if !bytes.Contains(frame.BrowserData, subAgentLaneEventMarker) {
		return nil
	}
	var event struct {
		Type                string                     `json:"type"`
		StreamID            string                     `json:"stream_id"`
		MessageID           string                     `json:"message_id"`
		ExecutionGeneration string                     `json:"execution_generation"`
		SIOEvent            string                     `json:"sio_event"`
		ResponseMetadata    map[string]json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(frame.BrowserData, &event) != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	if event.Type != subAgentLaneEventType {
		return nil
	}
	raw, ok := event.ResponseMetadata[subAgentLaneMetadataKey]
	if !ok {
		return nil
	}
	projectDatabaseID, valid := currentAgentDatabaseID(projectID)
	if !valid {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(ctx, tx, frame.Fence.ExecutionID, frame.Fence.Generation, projectDatabaseID)
	if err != nil {
		return err
	}
	if !bound || event.StreamID != binding.streamID || event.MessageID != binding.messageID ||
		event.ExecutionGeneration != binding.executionGeneration || event.SIOEvent != binding.sioEvent {
		return errors.New("sub-agent lane conflicts with immutable response binding")
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	groupID, err := lockCurrentAgentMessageGroup(ctx, tx, schema, frame.Fence.ExecutionID, binding)
	if err != nil {
		return err
	}
	// issue 1923: the frame's own child is the only lane this update can
	// touch, so only its one row is read and rewritten — never the whole
	// answer's lane set, which is what made the old whole-array rewrite
	// O(n^2) per turn.
	var entry subAgentLaneEntry
	if json.Unmarshal(raw, &entry) != nil {
		return nil
	}
	agentID, ok := laneID(entry.AgentID)
	if !ok {
		return nil
	}
	var existingRaw []byte
	var existing []subAgentLane
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT lane FROM %s WHERE message_group_id = $1 AND agent_id = $2`,
		schema+".chat_message_subagent_lanes"), groupID, agentID).Scan(&existingRaw)
	switch {
	case err == nil:
		existing = decodeSubAgentLanes(existingRaw)
	case errors.Is(err, pgx.ErrNoRows):
		existing = nil
	default:
		return fmt.Errorf("load sub-agent lane: %w", err)
	}
	lanes, changed := mergeSubAgentLanes(existing, raw)
	if !changed || len(lanes) != 1 {
		return nil
	}
	encoded, err := json.Marshal(lanes[0])
	if err != nil {
		return fmt.Errorf("encode sub-agent lane: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s (message_group_id, agent_id, ordinal, lane, updated_at)
VALUES ($1, $2, $3, $4::jsonb, clock_timestamp())
ON CONFLICT (message_group_id, agent_id)
DO UPDATE SET lane = EXCLUDED.lane, updated_at = EXCLUDED.updated_at`,
		schema+".chat_message_subagent_lanes"), groupID, agentID, lanes[0].Ordinal, encoded); err != nil {
		return fmt.Errorf("store sub-agent lane: %w", err)
	}
	return nil
}

// subAgentLanesByGroup reads every message group's sub-agent lanes
// (chat_message_subagent_lanes), folded by decodeAnswerSubAgentLanes
// (#1872 nesting), keyed by the chat_message_group.id the caller already
// has. A group with no lane rows is simply absent from the map — the
// callers below treat that the same as an empty metadata.subagents_v1.
//
// A fresh-install tenant (the E2E stack's `/elitea-migrate` with no flags,
// which runs Bootstrap and ApplyShared only — never the tenant history)
// never applies tenant/0170 and so has no table here at all: every caller
// of this function reaches it on EVERY transcript read, so the gap #599
// named for a missing join table applies to a missing TABLE the same way.
// 42P01 (undefined_table) and 3F000 (undefined_schema, a project whose
// tenant schema does not exist yet) read as "no lanes", exactly as a
// tenant that has the table but no rows for these groups.
func subAgentLanesByGroup(ctx context.Context, queryer pgxQueryer, s string, groupIDs []int) (map[int][]subAgentLane, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := queryer.Query(ctx, fmt.Sprintf(`
		SELECT message_group_id, lane FROM %s
		WHERE message_group_id = ANY($1)
		ORDER BY message_group_id, ordinal`, s+".chat_message_subagent_lanes"), groupIDs)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000") {
			return nil, nil
		}
		return nil, fmt.Errorf("conversations: list sub-agent lanes: %w", err)
	}
	defer rows.Close()

	raw := map[int][][]byte{}
	for rows.Next() {
		var groupID int
		var lane []byte
		if err := rows.Scan(&groupID, &lane); err != nil {
			return nil, fmt.Errorf("conversations: scan sub-agent lane: %w", err)
		}
		raw[groupID] = append(raw[groupID], lane)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversations: list sub-agent lanes: %w", err)
	}
	byGroup := make(map[int][]subAgentLane, len(raw))
	for groupID, laneRows := range raw {
		byGroup[groupID] = decodeAnswerSubAgentLanes(laneRows)
	}
	return byGroup, nil
}

// applySubAgentLanes sets (*metadata)["subagents_v1"] from the lanes table,
// overriding whatever an older written meta -> 'subagents_v1' value holds
// (the table is the lanes' owner since issue 1923; a frame is never again
// written to meta once this ships). A group with no lane rows is left
// exactly as its own meta reads: most answers never spawn a sub-agent, and
// setting the key to an empty list would turn "never had sub-agents" into
// "had sub-agents, now none" for every other reader of the same metadata
// map. *metadata is allocated when nil and there is something to carry: a
// row whose own meta was `{}` (unmarshalled to a nil map) must still be
// able to show the lanes the table (not that row) now owns.
func applySubAgentLanes(metadata *map[string]any, lanes []subAgentLane) {
	if len(lanes) == 0 {
		return
	}
	if *metadata == nil {
		*metadata = map[string]any{}
	}
	(*metadata)[subAgentLanesMetaKey] = lanes
}
