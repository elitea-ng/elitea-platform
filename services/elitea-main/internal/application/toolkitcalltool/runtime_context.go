package toolkitcalltool

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// RuntimeContext carries policy independently of toolkit settings and arguments.
// Credential values are never accepted in this immutable document.
type RuntimeContext struct {
	LLMConfiguration  json.RawMessage           `json:"llm_configuration,omitempty"`
	ToolkitSecurity   *guardrails.RuntimePolicy `json:"toolkit_security"`
	LLMModel          string                    `json:"llm_model,omitempty"`
	MCPTokenReference *MCPTokenReference        `json:"mcp_token_reference,omitempty"`
}

func validRuntimeContext(raw []byte) bool {
	if !validBoundedJSONObject(raw) {
		return false
	}
	var value RuntimeContext
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.ToolkitSecurity == nil {
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		return false
	}
	if len(value.LLMModel) > maxAdmissionStringBytes || !utf8.ValidString(value.LLMModel) || strings.ContainsAny(value.LLMModel, "\x00\r\n") || value.LLMModel != strings.TrimSpace(value.LLMModel) {
		return false
	}
	if value.MCPTokenReference != nil && !value.MCPTokenReference.valid() {
		return false
	}
	if !validModelSettings(value.LLMConfiguration) {
		return false
	}
	policy := value.ToolkitSecurity
	return policy.BlockedToolkits != nil && policy.BlockedTools != nil && policy.SensitiveTools != nil
}

// MCPTokenReference binds an opaque immutable grant to the saved toolkit resource.
type MCPTokenReference struct {
	Reference string `json:"reference"`
	Revision  int64  `json:"revision"`
	ToolkitID int64  `json:"toolkit_id"`
	Resource  string `json:"resource"`
}

func (r MCPTokenReference) valid() bool {
	return validMCPReference(r.Reference) && r.Revision > 0 && r.ToolkitID > 0 && len(r.Resource) > 0 && len(r.Resource) <= 4096 && !strings.ContainsAny(r.Resource, "\x00\r\n")
}
func validMCPReference(reference string) bool {
	if len(reference) != 43 {
		return false
	}
	for _, value := range reference {
		if !(value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-') {
			return false
		}
	}
	return true
}

func validModelSettings(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if len(raw) > 4096 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
		switch key {
		case "temperature":
			var number float64
			if json.Unmarshal(value, &number) != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 2 {
				return false
			}
		case "max_tokens":
			var number int64
			if json.Unmarshal(value, &number) != nil || number < -1 || number > 1048576 {
				return false
			}
		case "reasoning_effort":
			var text string
			if json.Unmarshal(value, &text) != nil {
				return false
			}
			switch text {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max":
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}
