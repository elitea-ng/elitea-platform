package storage

import "encoding/json"

func MatchCodeDebugPolicy(policy []byte, a CodeDebugAdmission) (string, error) {
	saved, err := MatchOriginalSavedCodeConfiguration(policy, a.ConfigurationJSON)
	if err != nil || !saved.Debug {
		return "", ErrContentUnauthorized
	}
	original, err := codeDebugJSONObject(policy)
	if err != nil || original["id"] != a.NodeID {
		return "", ErrContentUnauthorized
	}
	var source struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if json.Unmarshal(saved.Source, &source) != nil {
		return "", ErrContentUnauthorized
	}
	if source.Type == "fixed" && CodeDebugSHA256([]byte(source.Value)) != a.SourceSHA256 {
		return "", ErrContentUnauthorized
	}
	return saved.Language, nil
}

// CodeDebugPolicies retains only declarations eligible for the debug consumer.
func CodeDebugPolicies(instructions string) (map[string][]byte, error) {
	policies, err := SavedCodePolicies(instructions)
	if err != nil {
		return nil, err
	}
	for id, raw := range policies {
		value, err := codeDebugJSONObject(raw)
		if err != nil {
			return nil, err
		}
		if value["debug"] != true {
			delete(policies, id)
		}
	}
	return policies, nil
}
