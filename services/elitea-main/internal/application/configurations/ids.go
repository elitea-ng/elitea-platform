package configurations

import "strings"
import "strconv"

const MaxCurrentConfigurationIDs = 100

// ParseCurrentConfigurationIDs accepts comma-separated positive IDs. Empty text means no filter.
func ParseCurrentConfigurationIDs(raw string) ([]int32, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > MaxCurrentConfigurationIDs*12 {
		return nil, ErrInvalidCurrentConfigurationRequest
	}
	parts := strings.Split(raw, ",")
	if len(parts) > MaxCurrentConfigurationIDs {
		return nil, ErrInvalidCurrentConfigurationRequest
	}
	ids := make([]int32, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, ErrInvalidCurrentConfigurationRequest
		}
		for _, digit := range value {
			if digit < '0' || digit > '9' {
				return nil, ErrInvalidCurrentConfigurationRequest
			}
		}
		id, err := strconv.ParseInt(value, 10, 32)
		if err != nil || id <= 0 {
			return nil, ErrInvalidCurrentConfigurationRequest
		}
		ids = append(ids, int32(id))
	}
	return NormalizeCurrentConfigurationIDs(ids)
}

// NormalizeCurrentConfigurationIDs validates and copies IDs, retaining first occurrence order.
// Nil and empty slices both mean no filter.
func NormalizeCurrentConfigurationIDs(ids []int32) ([]int32, error) {
	if len(ids) > MaxCurrentConfigurationIDs {
		return nil, ErrInvalidCurrentConfigurationRequest
	}
	var result []int32
	seen := make(map[int32]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, ErrInvalidCurrentConfigurationRequest
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, nil
}
