package agentexecution

import (
	"encoding/json"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

// Admission replaces authored markers with Main-owned immutable request receipts.
//
// storedVersion is the version as saved, before Main froze its tools. Freezing
// drops a guardrail-blocked toolkit or one whose schema this runtime lacks, so a
// direct tool node naming such a toolkit is attached, just not runnable: its
// stored names count as attached and the Worker's own refusal stands. Without a
// stored version the toolkit check is skipped rather than guessed.
func freezeCurrentHTTPActionRequests(applicationID, versionID int64, version, storedVersion json.RawMessage) (json.RawMessage, error) {
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
		if names, ok := attachedToolkitNames(fields["tools"], storedVersion); ok {
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

// attachedToolkitNames answers every name a direct tool node may bind to: the
// frozen tools' toolkit_name plus each stored tool's toolkit_name and name. It
// answers false when either list does not decode or no stored version is
// known, and the toolkit check is then skipped rather than guessed (the Worker
// refuses such a snapshot on its own).
func attachedToolkitNames(frozen, storedVersion json.RawMessage) ([]string, bool) {
	var stored struct {
		Tools json.RawMessage `json:"tools"`
	}
	if len(storedVersion) == 0 || json.Unmarshal(storedVersion, &stored) != nil {
		return nil, false
	}
	var names []string
	for _, tools := range []json.RawMessage{frozen, stored.Tools} {
		if len(tools) == 0 || string(tools) == "null" {
			continue
		}
		var entries []struct {
			ToolkitName any `json:"toolkit_name"`
			Name        any `json:"name"`
		}
		if json.Unmarshal(tools, &entries) != nil {
			return nil, false
		}
		for _, entry := range entries {
			for _, name := range []any{entry.ToolkitName, entry.Name} {
				switch text := name.(type) {
				case nil:
				case string:
					names = append(names, text)
				default:
					return nil, false
				}
			}
		}
	}
	return names, true
}
