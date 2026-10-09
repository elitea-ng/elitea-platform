package agentexecution

import (
	"encoding/json"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
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
		// Refuse what the Worker would refuse anyway, but with the node and the
		// fix named: a node type this deployment does not run, or a direct tool
		// node whose toolkit the frozen version does not attach.
		admit := pipelinelimits.Check
		if names, ok := frozenToolkitNames(fields["tools"]); ok {
			admit = func(instructions string) error { return pipelinelimits.CheckStart(instructions, names) }
		}
		if err := admit(instructions); err != nil {
			return nil, err
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

// frozenToolkitNames reads the toolkit names of the version's frozen tools. A
// list that does not decode answers false, and the toolkit check is skipped
// rather than guessed (the Worker refuses such a snapshot on its own).
func frozenToolkitNames(tools json.RawMessage) ([]string, bool) {
	var entries []struct {
		ToolkitName string `json:"toolkit_name"`
	}
	if len(tools) == 0 || string(tools) == "null" {
		return nil, true
	}
	if json.Unmarshal(tools, &entries) != nil {
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.ToolkitName)
	}
	return names, true
}
