package applications

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentsoul"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// soulRefusal is the 400 body of a refused soul (ADR-0050 decision 1a).
type soulRefusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// applySoul reads `soul` from a version write body into v. An absent key
// leaves v alone; null clears the soul (Present.Soul with a nil Soul); an
// object is checked against AgentSoulV1 and stored as its canonical JSON. It
// writes the 400 and answers false on a refusal.
func applySoul(w http.ResponseWriter, body map[string]any, v *applications.Version) bool {
	raw, present := body["soul"]
	if !present {
		return true
	}
	soul, err := agentsoul.Normalize(raw)
	if err != nil {
		var refused *agentsoul.Error
		if errors.As(err, &refused) {
			writeJSON(w, http.StatusBadRequest, soulRefusal{Error: refused.Message, Code: refused.Code})
			return false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the soul schema could not be loaded"})
		return false
	}
	v.Soul = soul
	v.Present.Soul = true
	return true
}

// soulObject is the wire value of a stored soul: the object, or nil (JSON
// null) for none.
func soulObject(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil
	}
	return object
}

// soulValue is soulObject over a scanned nullable column.
func soulValue(column *string) any {
	if column == nil {
		return nil
	}
	return soulObject(json.RawMessage(*column))
}
