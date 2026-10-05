package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"reflect"
	"strconv"
	"strings"
)

const currentAgentCodeDebugKey = "code_debug_v1"

type currentAgentCodeDebug struct {
	Revision      int                                 `json:"revision"`
	OriginalVisit code.OriginalVisitRef               `json:"original_visit"`
	Attempt       uint16                              `json:"attempt"`
	ExecutionID   string                              `json:"execution_id"`
	Generation    string                              `json:"generation"`
	NodeID        string                              `json:"node_id"`
	ActivationID  string                              `json:"activation_id"`
	RequestSHA256 string                              `json:"request_sha256"`
	Status        string                              `json:"status"`
	Artifact      *storage.CodeDebugArtifactReference `json:"artifact,omitempty"`
}

func decodeCurrentAgentCodeDebug(raw any) (currentAgentCodeDebug, error) {
	invalid := errors.New("invalid Code debug trace")
	data, err := json.Marshal(raw)
	if err != nil || len(data) > 4096 {
		return currentAgentCodeDebug{}, invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var p currentAgentCodeDebug
	if d.Decode(&p) != nil || p.Revision != 1 || p.OriginalVisit.Validate() != nil || p.Attempt < 1 || p.Attempt > 16 || !codeTraceIdentity(p.ExecutionID, 256) || !codeTraceDecimal(p.Generation, true) || !codeTraceNodeID(p.NodeID) || !codeDebugDigest(p.ActivationID) || !codeDebugDigest(p.RequestSHA256) || (p.Status != "committed" && p.Status != "denied" && p.Status != "unavailable") || (p.Status == "committed") != (p.Artifact != nil) {
		return p, invalid
	}
	if a := p.Artifact; a != nil {
		if a.SchemaVersion != storage.CodeDebugArtifactSchema || a.ProjectID <= 0 || a.ProjectID > 2147483647 || a.Bucket != "code-debug" || a.MediaType != "application/json" || a.ByteLength < 1 || a.ByteLength > storage.MaxCodeDebugSnapshotBytes || !codeDebugDigest(a.SHA256) || len(a.Name) != 69 || !strings.HasSuffix(a.Name, ".json") || !codeDebugDigest(a.Name[:64]) {
			return p, invalid
		}
	}
	return p, nil
}
func codeDebugDigest(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
func (p currentAgentCodeDebug) runID() string {
	hash := sha256.New()
	hash.Write([]byte("elitea.graph.code.debug-trace.v1\x00"))
	for _, field := range []string{p.ExecutionID, p.Generation, p.OriginalVisit.VisitID, strconv.FormatUint(uint64(p.OriginalVisit.Revision), 10), p.OriginalVisit.DigestSHA256, strconv.FormatUint(uint64(p.Attempt), 10), p.ActivationID, p.RequestSHA256} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		hash.Write(length[:])
		hash.Write([]byte(field))
	}
	return "code-debug-" + hex.EncodeToString(hash.Sum(nil))
}
func currentAgentCodeDebugProof(entry map[string]any) (currentAgentCodeDebug, bool, error) {
	metadata := currentAgentMap(entry, "metadata")
	toolMetadata := currentAgentMap(currentAgentMap(entry, "tool_meta"), "metadata")
	raw, a := metadata[currentAgentCodeDebugKey]
	other, b := toolMetadata[currentAgentCodeDebugKey]
	if !a && !b {
		return currentAgentCodeDebug{}, false, nil
	}
	p, err := decodeCurrentAgentCodeDebug(raw)
	q, otherErr := decodeCurrentAgentCodeDebug(other)
	if !a || !b || err != nil || otherErr != nil || !reflect.DeepEqual(p, q) {
		return p, true, errors.New("conflicting Code debug metadata")
	}
	for _, m := range []map[string]any{metadata, toolMetadata} {
		if m["langgraph_node"] != p.NodeID || m["original_name"] != p.NodeID || m["node_type"] != "code" {
			return p, true, errors.New("conflicting Code debug node")
		}
	}
	return p, true, nil
}
func validateCurrentAgentCodeDebugDelta(ctx context.Context, tx sqlExecutor, delta currentAgentTraceDelta, frame outputapp.NodeEventFrame) error {
	for _, call := range delta.toolCalls {
		p, present, err := currentAgentCodeDebugProof(call.entry)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if p.ExecutionID != frame.Fence.ExecutionID || p.Generation != strconv.FormatUint(frame.Fence.Generation, 10) || call.key != p.runID() || call.entry["run_id"] != call.key || call.entry["tool_run_id"] != call.key || call.entry["tool_name"] != p.NodeID+" / debug export" || call.entry["tool_output"] != nil || call.entry["error"] != nil || currentAgentMap(call.entry, "tool_meta")["name"] != p.NodeID+" / debug export" || call.entry["finish_reason"] != "stop" {
			return errors.New("Code debug trace identity conflicts")
		}
		start, finish := parseCurrentAgentTime(call.entry["timestamp_start"]), parseCurrentAgentTime(call.entry["timestamp_finish"])
		if start == nil || finish == nil || !finish.Equal(*start) {
			return errors.New("Code debug completion time conflicts")
		}
		if input, ok := call.entry["tool_inputs"].(map[string]any); !ok || len(input) != 0 {
			return errors.New("Code debug trace contains inputs")
		}
		if p.Artifact == nil {
			continue
		}
		if strconv.FormatInt(p.Artifact.ProjectID, 10) != frame.ResourceProjectID {
			return errors.New("Code debug artifact project conflicts")
		}
		var key string
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT object_key,admission_json FROM elitea_runtime.code_debug_artifacts WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND original_generation=$4 AND original_visit_id=$5 AND original_visit_revision=$6 AND original_visit_digest=$7 AND state='committed'`, frame.TenantID, p.Artifact.ProjectID, p.ExecutionID, int64(frame.Fence.Generation), codeDebugBytes(p.OriginalVisit.VisitID), int64(p.OriginalVisit.Revision), codeDebugBytes(p.OriginalVisit.DigestSHA256)).Scan(&key, &raw); err != nil {
			return errors.New("Code debug receipt is unavailable")
		}
		var a storage.CodeDebugAdmission
		if json.Unmarshal(raw, &a) != nil || storage.ValidateCodeDebugAdmission(a) != nil || a.OriginalVisit != p.OriginalVisit || a.Attempt != p.Attempt || a.ActivationID != p.ActivationID || a.NodeID != p.NodeID || a.RequestSHA256 != p.RequestSHA256 || a.SnapshotSHA256 != p.Artifact.SHA256 || a.ByteLength != p.Artifact.ByteLength || key != p.Artifact.Name {
			return errors.New("Code debug artifact conflicts with committed receipt")
		}
	}
	return nil
}
func mergeCurrentAgentCodeDebug(previous, incoming map[string]any) (map[string]any, error) {
	old, a, err := currentAgentCodeDebugProof(previous)
	if err != nil {
		return nil, err
	}
	next, b, err := currentAgentCodeDebugProof(incoming)
	if err != nil {
		return nil, err
	}
	if !a && !b {
		return incoming, nil
	}
	if !b || !a && previous != nil {
		return nil, errors.New("Code debug trace cannot change kind")
	}
	if !a {
		return incoming, nil
	}
	if old.ExecutionID != next.ExecutionID || old.Generation != next.Generation || old.OriginalVisit != next.OriginalVisit || old.Attempt != next.Attempt || old.NodeID != next.NodeID || old.ActivationID != next.ActivationID || old.RequestSHA256 != next.RequestSHA256 {
		return nil, errors.New("Code debug replay identity conflicts")
	}
	if old.Artifact != nil {
		if next.Artifact != nil && !reflect.DeepEqual(old.Artifact, next.Artifact) {
			return nil, errors.New("Code debug artifact replay conflicts")
		}
		return previous, nil
	}
	return incoming, nil
}
func normalizedCurrentAgentCodeDebug(raw any) (any, bool) {
	p, err := decodeCurrentAgentCodeDebug(raw)
	if err != nil {
		return nil, false
	}
	data, _ := json.Marshal(p)
	result, err := decodeCurrentAgentJSONValue(data)
	return result, err == nil
}
