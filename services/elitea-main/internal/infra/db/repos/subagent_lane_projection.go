package repos

// The cloud sub-agent lane (ADR-0050 decision 4, #1823). The Rust worker
// streams each `spawn_agent` child's lifecycle as `agent_subagent` node
// events on the run's own output stream: its start (display name, avatar
// key, label), its own tool calls and results (summarised), its text and its
// end. Clients fold them into a lane under the `spawn_agent` row while the
// run streams. Main folds the same entries onto the answer's
// `meta.subagents_v1`, in the transaction that accepts the frame, so a
// reloaded transcript shows the settled lane: the spawn record of #1614
// (`elitea_runtime.ephemeral_agents`) keeps no tool calls or text.
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
	"strings"
	"unicode/utf8"

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

// subAgentLane is one child as the answer's meta keeps it.
type subAgentLane struct {
	AgentID      string             `json:"agent_id"`
	ParentCallID string             `json:"parent_call_id"`
	Ordinal      int                `json:"ordinal"`
	Label        string             `json:"label,omitempty"`
	DisplayName  string             `json:"display_name,omitempty"`
	Avatar       string             `json:"avatar,omitempty"`
	Status       string             `json:"status"`
	Items        []subAgentLaneItem `json:"items"`
	Steps        *int64             `json:"steps,omitempty"`
	InputTokens  *int64             `json:"input_tokens,omitempty"`
	OutputTokens *int64             `json:"output_tokens,omitempty"`
	Result       string             `json:"result,omitempty"`
	Error        string             `json:"error,omitempty"`
}

// subAgentLaneEntry is one `subagent_v1` entry as the worker sends it.
type subAgentLaneEntry struct {
	Kind         string `json:"kind"`
	AgentID      string `json:"agent_id"`
	ParentCallID string `json:"parent_call_id"`
	Ordinal      int    `json:"ordinal"`
	Label        string `json:"label"`
	DisplayName  string `json:"display_name"`
	Avatar       string `json:"avatar"`
	CallID       string `json:"call_id"`
	Tool         string `json:"tool"`
	ArgsSummary  string `json:"args_summary"`
	OK           *bool  `json:"ok"`
	Summary      string `json:"summary"`
	Text         string `json:"text"`
	Status       string `json:"status"`
	Steps        *int64 `json:"steps"`
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	Result       string `json:"result"`
	Error        string `json:"error"`
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
		return append(lanes, subAgentLane{
			AgentID: agentID, ParentCallID: parent, Ordinal: entry.Ordinal,
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

// decodeSubAgentLanes reads the lanes the answer's meta already keeps; a
// value that is not a lane list reads as none.
func decodeSubAgentLanes(raw []byte) []subAgentLane {
	var lanes []subAgentLane
	if len(raw) == 0 || json.Unmarshal(raw, &lanes) != nil {
		return nil
	}
	return lanes
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
	var existing []byte
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(meta -> '%s', '[]'::jsonb) FROM %s WHERE id = $1`,
		subAgentLanesMetaKey, schema+".chat_message_group"), groupID).Scan(&existing); err != nil {
		return fmt.Errorf("load sub-agent lanes: %w", err)
	}
	lanes, changed := mergeSubAgentLanes(decodeSubAgentLanes(existing), raw)
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(lanes)
	if err != nil {
		return fmt.Errorf("encode sub-agent lanes: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s
SET meta = jsonb_set(COALESCE(meta, '{}'::jsonb), '{%s}', $2::jsonb, TRUE),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, schema+".chat_message_group", subAgentLanesMetaKey), groupID, encoded); err != nil {
		return fmt.Errorf("store sub-agent lanes: %w", err)
	}
	return nil
}
