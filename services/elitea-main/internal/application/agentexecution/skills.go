package agentexecution

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	currentMaxInvokedSkills               = 5
	currentInstructionReferencedSkillHint = "Referenced by name in your agent instructions: load this skill whenever " +
		"the situation those instructions specify for it applies — never follow " +
		"the reference from its name alone, load it first."
)

type currentSkillProjection struct {
	userInput      string
	versionDetails json.RawMessage
	invoked        json.RawMessage
	applied        json.RawMessage
	attached       json.RawMessage
}

type currentSkillCandidate struct {
	name  string
	lower string
	skill map[string]any
}

type currentSkillMention struct {
	start int
	end   int
	name  string
}

// projectCurrentApplicationSkills preserves the current Core skill boundary:
// message-level ~skill references are consumed into invoked/applied channels,
// while non-pipeline application skills remain available for the SDK-owned
// load_skill tool. The stored chat question is intentionally left untouched;
// only the immutable worker input receives the de-sigiled text.
func projectCurrentApplicationSkills(
	userInput string,
	versionDetails json.RawMessage,
) (currentSkillProjection, error) {
	version, err := decodeCurrentApplicationVersion(versionDetails)
	if err != nil {
		return currentSkillProjection{}, ErrUnsupportedCurrentAgentStart
	}
	attached, ok := currentAttachedSkillObjects(version["skills"])
	if !ok {
		return currentSkillProjection{}, ErrUnsupportedCurrentAgentStart
	}

	cleanedInput, invoked := consumeCurrentInvokedSkills(userInput, attached)
	if strings.TrimSpace(cleanedInput) == "" {
		cleanedInput = "continue"
	}
	applied := make([]map[string]any, 0, len(invoked))
	for _, skill := range invoked {
		applied = append(applied, currentSkillAuthorityProjection(map[string]any{
			"skill_id":         skill["skill_id"],
			"skill_version_id": skill["skill_version_id"],
			"name":             skill["name"],
			"icon_meta":        skill["icon_meta"],
		}, skill))
	}

	disclosable := make([]map[string]any, 0, len(attached))
	if version["agent_type"] != "pipeline" {
		instructionSkills := []map[string]any(nil)
		if instructions, ok := version["instructions"].(string); ok && instructions != "" {
			cleanedInstructions, selected := consumeCurrentInvokedSkills(instructions, attached)
			if cleanedInstructions != instructions {
				version["instructions"] = cleanedInstructions
			}
			instructionSkills = selected
		}
		referenced := make(map[string]struct{}, len(instructionSkills))
		for _, skill := range instructionSkills {
			referenced[currentSkillIdentity(skill["skill_id"])] = struct{}{}
		}
		for _, skill := range attached {
			name, nameOK := skill["name"].(string)
			instructions, instructionsOK := skill["instructions"].(string)
			if !nameOK || name == "" || !instructionsOK || strings.TrimSpace(instructions) == "" {
				continue
			}
			description, _ := skill["description"].(string)
			if _, ok := referenced[currentSkillIdentity(skill["skill_id"])]; ok {
				description = strings.TrimSpace(strings.TrimSpace(description) + " " + currentInstructionReferencedSkillHint)
			}
			disclosable = append(disclosable, currentSkillAuthorityProjection(map[string]any{
				"skill_id":         skill["skill_id"],
				"skill_version_id": skill["skill_version_id"],
				"name":             name,
				"description":      nullableCurrentSkillDescription(skill["description"], description),
				"icon_meta":        skill["icon_meta"],
				"instructions":     instructions,
			}, skill))
		}
	}

	encodedVersion, err := json.Marshal(version)
	if err != nil || !validJSONObject(encodedVersion) {
		return currentSkillProjection{}, ErrUnsupportedCurrentAgentStart
	}
	encodedInvoked, err := json.Marshal(invoked)
	if err != nil {
		return currentSkillProjection{}, ErrUnsupportedCurrentAgentStart
	}
	encodedApplied, err := json.Marshal(applied)
	if err != nil {
		return currentSkillProjection{}, ErrUnsupportedCurrentAgentStart
	}
	encodedAttached, err := json.Marshal(disclosable)
	if err != nil {
		return currentSkillProjection{}, ErrUnsupportedCurrentAgentStart
	}
	return currentSkillProjection{
		userInput: cleanedInput, versionDetails: encodedVersion,
		invoked: encodedInvoked, applied: encodedApplied, attached: encodedAttached,
	}, nil
}

func currentAttachedSkillObjects(value any) ([]map[string]any, bool) {
	if value == nil {
		return []map[string]any{}, true
	}
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		skill, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		result = append(result, skill)
	}
	return result, true
}

func consumeCurrentInvokedSkills(
	text string,
	attached []map[string]any,
) (string, []map[string]any) {
	if text == "" || len(attached) == 0 || !strings.ContainsRune(text, '~') {
		return text, []map[string]any{}
	}
	candidates := make([]currentSkillCandidate, 0, len(attached))
	seenNames := make(map[string]struct{}, len(attached))
	for _, skill := range attached {
		name, ok := skill["name"].(string)
		if !ok || name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if _, exists := seenNames[lower]; exists {
			continue
		}
		seenNames[lower] = struct{}{}
		candidates = append(candidates, currentSkillCandidate{name: name, lower: lower, skill: skill})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return utf8.RuneCountInString(candidates[left].name) > utf8.RuneCountInString(candidates[right].name)
	})
	if len(candidates) == 0 {
		return text, []map[string]any{}
	}

	runes := []rune(text)
	mentions := make([]currentSkillMention, 0, len(candidates))
	for index := 0; index < len(runes); {
		if runes[index] != '~' || (index > 0 && (currentSkillNameRune(runes[index-1]) || runes[index-1] == '~')) {
			index++
			continue
		}
		start := index + 1
		matched := false
		for _, candidate := range candidates {
			candidateRunes := []rune(candidate.name)
			end := start + len(candidateRunes)
			if end > len(runes) || !strings.EqualFold(string(runes[start:end]), candidate.name) {
				continue
			}
			if end < len(runes) && currentSkillNameRune(runes[end]) {
				continue
			}
			mentions = append(mentions, currentSkillMention{start: index, end: end, name: candidate.name})
			index = end
			matched = true
			break
		}
		if !matched {
			index++
		}
	}
	if len(mentions) == 0 {
		return text, []map[string]any{}
	}

	cleaned := make([]rune, 0, len(runes))
	cursor := 0
	for _, mention := range mentions {
		cleaned = append(cleaned, runes[cursor:mention.start]...)
		cleaned = append(cleaned, []rune(mention.name)...)
		cursor = mention.end
	}
	cleaned = append(cleaned, runes[cursor:]...)

	byName := make(map[string]map[string]any, len(candidates))
	for _, candidate := range candidates {
		byName[candidate.lower] = candidate.skill
	}
	invoked := make([]map[string]any, 0, currentMaxInvokedSkills)
	seenInvoked := make(map[string]struct{}, currentMaxInvokedSkills)
	for _, mention := range mentions {
		key := strings.ToLower(mention.name)
		if _, exists := seenInvoked[key]; exists {
			continue
		}
		seenInvoked[key] = struct{}{}
		skill := byName[key]
		instructions, ok := skill["instructions"].(string)
		if !ok || strings.TrimSpace(instructions) == "" {
			continue
		}
		invoked = append(invoked, currentSkillAuthorityProjection(map[string]any{
			"skill_id":         skill["skill_id"],
			"skill_version_id": skill["skill_version_id"],
			"name":             skill["name"],
			"version_name":     skill["version_name"],
			"icon_meta":        skill["icon_meta"],
			"instructions":     instructions,
		}, skill))
		if len(invoked) == currentMaxInvokedSkills {
			break
		}
	}
	return string(cleaned), invoked
}

func currentSkillNameRune(value rune) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' ||
		value >= 'a' && value <= 'z' || value == '-'
}

func currentSkillIdentity(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func nullableCurrentSkillDescription(original any, resolved string) any {
	if original == nil && resolved == "" {
		return nil
	}
	return resolved
}

func currentSkillAuthorityProjection(projected, source map[string]any) map[string]any {
	for _, key := range []string{"id", "revision", "scope"} {
		if value, ok := source[key]; ok {
			projected[key] = value
		}
	}
	return projected
}
