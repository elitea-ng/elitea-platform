package repos

const maxAgentModelOutputBytes = 4 * 1024 * 1024

func mergeAgentModelStep(previous, incoming map[string]any) (map[string]any, error) {
	result := cloneCurrentAgentMap(previous)
	for key, value := range incoming {
		result[key] = value
	}
	for _, field := range []string{"text", "thinking"} {
		key := field + "_chunk_v1"
		if _, ok := incoming[key]; !ok {
			continue
		}
		merged, err := mergeAgentTextChunk(previous, incoming, field, key, maxAgentModelOutputBytes, false)
		if err != nil {
			return nil, err
		}
		result[field] = merged[field]
		result[key] = merged[key]
	}
	return result, nil
}
