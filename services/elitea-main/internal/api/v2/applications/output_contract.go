package applications

import (
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
)

// outputContractRefusal is the typed 422 body of a refused output contract
// (ADR-0043 decision 2, #1500).
type outputContractRefusal struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
	NodeID string `json:"node_id,omitempty"`
}

// checkOutputContracts checks the output contracts a version save carries:
// body.meta.output_contract (an agent) and the output_contract of every LLM
// node of body.instructions (a pipeline). It answers false when it wrote the
// 422.
func checkOutputContracts(w http.ResponseWriter, body map[string]any) bool {
	if body == nil {
		return true
	}
	var err error
	if meta, _ := body["meta"].(map[string]any); meta != nil {
		err = outputcontract.Check(meta[outputcontract.MetaKey])
	}
	if instructions, _ := body["instructions"].(string); err == nil && instructions != "" {
		err = outputcontract.CheckPipeline(instructions)
	}
	if err == nil {
		return true
	}
	var refusal *outputcontract.Error
	if !errors.As(err, &refusal) {
		return true
	}
	writeJSON(w, http.StatusUnprocessableEntity, outputContractRefusal{
		Error:  refusal.Error(),
		Code:   outputcontract.Code,
		Reason: refusal.Reason,
		NodeID: refusal.NodeID,
	})
	return false
}
