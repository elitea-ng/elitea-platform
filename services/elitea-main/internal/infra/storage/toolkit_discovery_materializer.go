package storage

import "context"

// Discovery reuses the agent resolver for prebuilt endpoints and frozen owners.
// The admitted job supplies the toolkit type. The caller cannot replace it.
func (m *CurrentConfigurationsMaterializer) materializeToolkitDiscovery(ctx context.Context, projectID, actorID int32, toolkitType string, source []byte, maxBytes int64) ([]byte, error) {
	settings, err := decodeCurrentMaterializationObject(source)
	if err != nil {
		return nil, ErrContentRejected
	}
	tools := []any{map[string]any{"type": toolkitType, "settings": settings}}
	walker := currentFrozenConfigurationWalker{unsecreter: m.unsecreter}
	if err := m.materializeCurrentAgentTools(ctx, projectID, actorID, tools, &walker); err != nil {
		return nil, currentMaterializationError(ctx, err)
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		return nil, ErrContentRejected
	}
	materialized, ok := tool["settings"].(map[string]any)
	if !ok {
		return nil, ErrContentRejected
	}
	return encodeCurrentMaterializationObject(materialized, maxBytes)
}
