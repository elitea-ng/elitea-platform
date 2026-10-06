package repos

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

const currentAgentCodeTraceKey = "code_lifecycle_v1"

type currentAgentCodeLifecycle struct {
	Revision      int    `json:"revision"`
	ExecutionID   string `json:"execution_id"`
	Generation    string `json:"generation"`
	ActivationID  string `json:"activation_id"`
	NodeID        string `json:"node_id"`
	GraphThreadID string `json:"graph_thread_id"`
	GraphStep     string `json:"graph_step"`
	Language      string `json:"language"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
}

func decodeCurrentAgentCodeLifecycle(raw any) (currentAgentCodeLifecycle, error) {
	invalid := errors.New("invalid Code lifecycle trace metadata")
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > 2048 {
		return currentAgentCodeLifecycle{}, invalid
	}
	var proof currentAgentCodeLifecycle
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&proof) != nil || proof.Revision != 1 || !codeTraceIdentity(proof.ExecutionID, 256) ||
		!codeTraceDecimal(proof.Generation, true) || !codeTraceDecimal(proof.GraphStep, false) ||
		!codeTraceIdentity(proof.GraphThreadID, 512) || !codeTraceNodeID(proof.NodeID) || len(proof.ActivationID) != 64 ||
		strings.Trim(proof.ActivationID, "0123456789abcdef") != "" ||
		(proof.Language != "python" && proof.Language != "javascript" && proof.Language != "typescript" && proof.Language != "rust") ||
		(proof.Phase != "preparation" && proof.Phase != "hydration" && proof.Phase != "execution") ||
		(proof.Status != "started" && proof.Status != "completed" && proof.Status != "failed") {
		return currentAgentCodeLifecycle{}, invalid
	}
	return proof, nil
}
func codeTraceIdentity(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}
func codeTraceDecimal(value string, positive bool) bool {
	number, err := strconv.ParseUint(value, 10, 64)
	return err == nil && (!positive || number > 0) && strconv.FormatUint(number, 10) == value
}
func codeTraceNodeID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, b := range []byte(value) {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && !strings.ContainsRune("_-.:", rune(b)) {
			return false
		}
	}
	return true
}
func (proof currentAgentCodeLifecycle) runID() string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("elitea.graph.code.trace.v1\x00"))
	for _, field := range []string{proof.ExecutionID, proof.Generation, proof.ActivationID, proof.Phase} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(field))
	}
	return "code-" + hex.EncodeToString(hash.Sum(nil))
}
func currentAgentCodeProof(entry map[string]any) (currentAgentCodeLifecycle, bool, error) {
	metadata := currentAgentMap(entry, "metadata")
	raw, present := metadata[currentAgentCodeTraceKey]
	toolMetadata := currentAgentMap(currentAgentMap(entry, "tool_meta"), "metadata")
	alternate, otherPresent := toolMetadata[currentAgentCodeTraceKey]
	if !present && !otherPresent {
		return currentAgentCodeLifecycle{}, false, nil
	}
	proof, err := decodeCurrentAgentCodeLifecycle(raw)
	other, otherErr := decodeCurrentAgentCodeLifecycle(alternate)
	if err != nil || otherErr != nil || !present || !otherPresent || proof != other {
		return currentAgentCodeLifecycle{}, true, errors.New("conflicting Code lifecycle trace metadata")
	}
	for _, values := range []map[string]any{metadata, toolMetadata} {
		if values["langgraph_node"] != proof.NodeID || values["original_name"] != proof.NodeID ||
			values["node_type"] != "code" || values["language"] != proof.Language {
			return currentAgentCodeLifecycle{}, true, errors.New("conflicting Code lifecycle node metadata")
		}
	}
	return proof, true, nil
}

// Compare producer identity with the authenticated frame before any trace write.
func validateCurrentAgentCodeTraceDelta(delta currentAgentTraceDelta, frame outputapp.NodeEventFrame) error {
	for _, call := range delta.toolCalls {
		proof, present, err := currentAgentCodeProof(call.entry)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if proof.ExecutionID != frame.Fence.ExecutionID || proof.Generation != strconv.FormatUint(frame.Fence.Generation, 10) {
			return errors.New("code lifecycle trace conflicts with signed execution identity")
		}
		if err := validateCurrentAgentCodeEntry(call.key, call.entry, proof); err != nil {
			return err
		}
	}
	return nil
}
func validateCurrentAgentCodeEntry(key string, entry map[string]any, proof currentAgentCodeLifecycle) error {
	invalid := errors.New("invalid Code lifecycle trace entry")
	if key != proof.runID() || entry["run_id"] != key || entry["tool_run_id"] != key ||
		entry["tool_name"] != proof.NodeID+" / "+proof.Phase || currentAgentString(entry["tool_output"]) != "" {
		return invalid
	}
	if inputs, ok := entry["tool_inputs"].(map[string]any); !ok || len(inputs) != 0 {
		return invalid
	}
	started := parseCurrentAgentTime(entry["timestamp_start"])
	finished := parseCurrentAgentTime(entry["timestamp_finish"])
	if started == nil {
		return invalid
	}
	if proof.Status == "started" {
		if finished != nil || currentAgentTruthy(entry["error"]) || currentAgentString(entry["finish_reason"]) != "" {
			return invalid
		}
	} else {
		if finished == nil || finished.Before(*started) {
			return invalid
		}
		if proof.Status == "completed" && (currentAgentTruthy(entry["error"]) || entry["finish_reason"] != "stop") {
			return invalid
		}
		if proof.Status == "failed" && (entry["error"] != "The Code phase could not be completed." || entry["finish_reason"] != "error") {
			return invalid
		}
	}
	return nil
}

// A repeated start cannot erase a stored terminal phase.
func mergeCurrentAgentCodeTrace(previous, incoming map[string]any) (map[string]any, error) {
	old, oldPresent, err := currentAgentCodeProof(previous)
	if err != nil {
		return nil, err
	}
	next, nextPresent, err := currentAgentCodeProof(incoming)
	if err != nil {
		return nil, err
	}
	if !oldPresent && !nextPresent {
		return incoming, nil
	}
	if !nextPresent {
		return nil, errors.New("code lifecycle trace cannot change kind")
	}
	if !oldPresent {
		if previous != nil {
			return nil, errors.New("code lifecycle trace cannot replace another trace")
		}
		return incoming, nil
	}
	oldStatus, nextStatus := old.Status, next.Status
	old.Status, next.Status = "", ""
	if old != next {
		return nil, errors.New("code lifecycle replay identity conflicts")
	}
	if oldStatus != "started" {
		if nextStatus != "started" && nextStatus != oldStatus {
			return nil, errors.New("code lifecycle terminal result conflicts")
		}
		return previous, nil
	}
	if nextStatus == "started" {
		return previous, nil
	}
	result := cloneCurrentAgentMap(incoming)
	result["timestamp_start"] = previous["timestamp_start"]
	// Preserve the first observed start across recovery. This is display evidence.
	if start, finish := parseCurrentAgentTime(result["timestamp_start"]), parseCurrentAgentTime(result["timestamp_finish"]); start != nil && finish != nil && finish.Before(*start) {
		return nil, errors.New("code lifecycle timestamps conflict")
	}
	return result, nil
}

func normalizedCurrentAgentCodeLifecycle(raw any) (any, bool) {
	proof, err := decodeCurrentAgentCodeLifecycle(raw)
	if err != nil {
		return nil, false
	}
	// Keep a structural object for the existing attrs and replay adapters.
	encoded, _ := json.Marshal(proof)
	normalized, err := decodeCurrentAgentJSONValue(encoded)
	return normalized, err == nil
}
