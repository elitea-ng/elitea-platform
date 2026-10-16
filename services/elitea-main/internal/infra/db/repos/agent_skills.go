package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	currentAgentMaxPersistedInvokedSkills = 64
	currentAgentMaxSkillNameRunes         = 256
)

type currentAgentInvokedSkill struct {
	SkillID  int64           `json:"skill_id"`
	Name     string          `json:"name"`
	IconMeta json.RawMessage `json:"icon_meta"`
}

// decodeCurrentAgentInvokedSkills admits only the compact UI contract. Worker-
// only progressive-disclosure fields such as instructions and version details
// are intentionally discarded at the persistence boundary.
func decodeCurrentAgentInvokedSkills(raw json.RawMessage) ([]currentAgentInvokedSkill, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []currentAgentInvokedSkill{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var values []map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, errors.New("decode current agent invoked skills")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("current agent invoked skills have trailing data")
	}
	if len(values) > currentAgentMaxPersistedInvokedSkills {
		return nil, errors.New("current agent invoked skills exceed the persistence limit")
	}

	result := make([]currentAgentInvokedSkill, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		skillID, ok := currentAgentPositiveJSONInteger(value["skill_id"])
		name, nameOK := value["name"].(string)
		name = strings.TrimSpace(name)
		if !ok || !nameOK || name == "" || !utf8.ValidString(name) ||
			utf8.RuneCountInString(name) > currentAgentMaxSkillNameRunes ||
			strings.ContainsRune(name, '\x00') {
			return nil, errors.New("current agent invoked skill is invalid")
		}
		identity := strings.ToLower(name)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}

		iconMeta, err := compactCurrentAgentSkillIcon(value["icon_meta"])
		if err != nil {
			return nil, err
		}
		result = append(result, currentAgentInvokedSkill{
			SkillID: skillID, Name: name, IconMeta: iconMeta,
		})
	}
	return result, nil
}

func persistCurrentAgentTraceInvokedSkills(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	messageGroupID int64,
	incoming json.RawMessage,
) error {
	var existing []byte
	if err := tx.QueryRow(
		ctx,
		fmt.Sprintf(`SELECT COALESCE(meta -> 'invoked_skills', '[]'::jsonb) FROM %s WHERE id = $1`,
			schema+".chat_message_group"),
		messageGroupID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("load current agent invoked skills: %w", err)
	}
	merged, err := mergeCurrentAgentInvokedSkills(json.RawMessage(existing), incoming)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(
		ctx,
		fmt.Sprintf(`
UPDATE %s
SET meta = jsonb_set(COALESCE(meta, '{}'::jsonb), '{invoked_skills}', $2::jsonb, TRUE),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, schema+".chat_message_group"),
		messageGroupID,
		[]byte(merged),
	)
	if err != nil {
		return fmt.Errorf("persist current agent invoked skills: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("current agent invoked skills row was not updated")
	}
	return nil
}

func currentAgentPositiveJSONInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil && parsed > 0
}

func compactCurrentAgentSkillIcon(value any) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage("null"), nil
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("current agent invoked skill icon metadata is invalid")
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > currentAgentTraceMaxIconBytes {
		return nil, errors.New("current agent invoked skill icon metadata is invalid")
	}
	return encoded, nil
}

func mergeCurrentAgentInvokedSkills(existingRaw, incomingRaw json.RawMessage) (json.RawMessage, error) {
	existing, err := decodeCurrentAgentInvokedSkills(existingRaw)
	if err != nil {
		return nil, errors.New("decode persisted current agent invoked skills")
	}
	incoming, err := decodeCurrentAgentInvokedSkills(incomingRaw)
	if err != nil {
		return nil, err
	}
	merged := make([]currentAgentInvokedSkill, 0, len(existing)+len(incoming))
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	for _, source := range [][]currentAgentInvokedSkill{incoming, existing} {
		for _, skill := range source {
			if len(merged) >= currentAgentMaxPersistedInvokedSkills {
				break
			}
			identity := strings.ToLower(strings.TrimSpace(skill.Name))
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			merged = append(merged, skill)
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, errors.New("encode current agent invoked skills")
	}
	return encoded, nil
}
