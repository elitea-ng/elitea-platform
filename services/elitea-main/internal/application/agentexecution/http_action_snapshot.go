package agentexecution

import (
	"encoding/json"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
)

// Admission replaces authored markers with Main-owned immutable request receipts.
func freezeCurrentHTTPActionRequests(applicationID, versionID int64, version json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(version, &fields) != nil || fields == nil {
		return nil, app.ErrInvalid
	}
	delete(fields, "http_action_snapshot")
	var kind string
	if json.Unmarshal(fields["agent_type"], &kind) != nil {
		kind = ""
	}
	if kind == "pipeline" {
		var instructions string
		if json.Unmarshal(fields["instructions"], &instructions) != nil {
			return nil, app.ErrInvalid
		}
		snapshot, err := app.FreezeSnapshot(applicationID, versionID, instructions)
		if err != nil {
			return nil, err
		}
		if snapshot != nil {
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				return nil, err
			}
			fields["http_action_snapshot"] = encoded
		}
	}
	return json.Marshal(fields)
}
