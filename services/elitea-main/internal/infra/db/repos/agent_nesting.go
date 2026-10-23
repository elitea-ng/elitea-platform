package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

const (
	currentMaxAgentNestingTiers      = 3
	currentMaxApplicationNestingHops = 25
	currentMaxNestedSkillNameRunes   = 256
)

var errInvalidCurrentApplicationNesting = errors.New("invalid current application nesting")

type currentApplicationNestingQuerier interface {
	ResolveCurrentApplicationNestingNode(
		context.Context,
		int32,
	) (sqlcgen.ResolveCurrentApplicationNestingNodeRow, error)
}

type currentApplicationNestingKey struct {
	applicationID int32
	versionID     int32
}

type currentApplicationNestingReference struct {
	ToolID               int32           `json:"tool_id"`
	ToolName             *string         `json:"tool_name"`
	ApplicationID        json.RawMessage `json:"application_id"`
	ApplicationVersionID json.RawMessage `json:"application_version_id"`
}

type currentApplicationNestingNode struct {
	versionID     int32
	applicationID int32
	agentType     string
	skills        []currentApplicationNestingSkill
	children      []currentApplicationNestingReference
}

type currentApplicationNestingSkill struct {
	SkillID  int32           `json:"skill_id"`
	Name     string          `json:"name"`
	IconMeta json.RawMessage `json:"icon_meta"`
}

type currentApplicationSkillRegistry struct {
	ApplicationID        int32                            `json:"application_id"`
	ApplicationVersionID int32                            `json:"application_version_id"`
	ApplicationName      string                           `json:"application_name"`
	Skills               []currentApplicationNestingSkill `json:"skills"`
}

type currentApplicationNestingResolution struct {
	root       currentApplicationNestingNode
	registries []currentApplicationSkillRegistry
}

func validateCurrentApplicationNesting(
	ctx context.Context,
	queries currentApplicationNestingQuerier,
	versionID int32,
	startDepth int,
) error {
	_, err := resolveCurrentApplicationNesting(ctx, queries, versionID, startDepth)
	return err
}

func resolveCurrentApplicationNesting(
	ctx context.Context,
	queries currentApplicationNestingQuerier,
	versionID int32,
	startDepth int,
) (currentApplicationNestingResolution, error) {
	if ctx == nil || queries == nil || versionID <= 0 || startDepth <= 0 {
		return currentApplicationNestingResolution{}, errInvalidCurrentApplicationNesting
	}
	cache := make(map[int32]currentApplicationNestingNode)
	load := func(versionID int32) (currentApplicationNestingNode, error) {
		if node, ok := cache[versionID]; ok {
			return node, nil
		}
		row, err := queries.ResolveCurrentApplicationNestingNode(ctx, versionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return currentApplicationNestingNode{}, fmt.Errorf(
				"%w: application version %d does not exist",
				errInvalidCurrentApplicationNesting,
				versionID,
			)
		}
		if err != nil {
			return currentApplicationNestingNode{}, fmt.Errorf(
				"resolve application nesting node %d: %w",
				versionID,
				err,
			)
		}
		node, err := decodeCurrentApplicationNestingNode(row)
		if err != nil {
			return currentApplicationNestingNode{}, err
		}
		cache[versionID] = node
		return node, nil
	}

	root, err := load(versionID)
	if err != nil {
		return currentApplicationNestingResolution{}, err
	}
	agentTiers := startDepth - 1 + currentApplicationAgentTierContribution(root.agentType)
	path := make(map[currentApplicationNestingKey]struct{})
	registries := make([]currentApplicationSkillRegistry, 0)
	registered := make(map[currentApplicationNestingKey]struct{})
	for _, child := range root.children {
		if err := walkCurrentApplicationNesting(
			ctx,
			load,
			child,
			agentTiers,
			1,
			path,
			&registries,
			registered,
		); err != nil {
			return currentApplicationNestingResolution{}, err
		}
	}
	return currentApplicationNestingResolution{root: root, registries: registries}, nil
}

func walkCurrentApplicationNesting(
	ctx context.Context,
	load func(int32) (currentApplicationNestingNode, error),
	reference currentApplicationNestingReference,
	agentTiers int,
	rawDepth int,
	path map[currentApplicationNestingKey]struct{},
	registries *[]currentApplicationSkillRegistry,
	registered map[currentApplicationNestingKey]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	applicationID, validApplicationID := positiveCurrentApplicationNestingInteger(reference.ApplicationID)
	versionID, validVersionID := positiveCurrentApplicationNestingInteger(reference.ApplicationVersionID)
	if reference.ToolID <= 0 || !validApplicationID || !validVersionID {
		return fmt.Errorf(
			"%w: application tool %d has an incomplete child reference",
			errInvalidCurrentApplicationNesting,
			reference.ToolID,
		)
	}
	key := currentApplicationNestingKey{applicationID: applicationID, versionID: versionID}
	if _, exists := path[key]; exists {
		return fmt.Errorf(
			"%w: circular reference to application %d version %d",
			errInvalidCurrentApplicationNesting,
			applicationID,
			versionID,
		)
	}
	if rawDepth > currentMaxApplicationNestingHops {
		return fmt.Errorf(
			"%w: application nesting exceeds %d raw hops",
			errInvalidCurrentApplicationNesting,
			currentMaxApplicationNestingHops,
		)
	}

	node, err := load(versionID)
	if err != nil {
		return err
	}
	if node.applicationID != applicationID {
		return fmt.Errorf(
			"%w: application version %d belongs to application %d, not %d",
			errInvalidCurrentApplicationNesting,
			versionID,
			node.applicationID,
			applicationID,
		)
	}
	currentAgentTiers := agentTiers + currentApplicationAgentTierContribution(node.agentType)
	if node.agentType != "pipeline" && len(node.children) > 0 &&
		currentAgentTiers >= currentMaxAgentNestingTiers {
		return fmt.Errorf(
			"%w: agent version %d is a container at tier %d",
			errInvalidCurrentApplicationNesting,
			versionID,
			currentAgentTiers,
		)
	}

	path[key] = struct{}{}
	defer delete(path, key)
	if len(node.skills) > 0 {
		name := ""
		if reference.ToolName != nil {
			name = strings.TrimSpace(*reference.ToolName)
		}
		if name == "" || !utf8.ValidString(name) ||
			utf8.RuneCountInString(name) > currentMaxNestedSkillNameRunes ||
			strings.ContainsRune(name, '\x00') {
			return fmt.Errorf(
				"%w: application version %d has skills without a valid tool name",
				errInvalidCurrentApplicationNesting,
				versionID,
			)
		}
		if _, exists := registered[key]; !exists {
			registered[key] = struct{}{}
			*registries = append(*registries, currentApplicationSkillRegistry{
				ApplicationID: applicationID, ApplicationVersionID: versionID,
				ApplicationName: name, Skills: cloneCurrentApplicationNestingSkills(node.skills),
			})
		}
	}
	for _, child := range node.children {
		if err := walkCurrentApplicationNesting(
			ctx,
			load,
			child,
			currentAgentTiers,
			rawDepth+1,
			path,
			registries,
			registered,
		); err != nil {
			return err
		}
	}
	return nil
}

func decodeCurrentApplicationNestingNode(
	row sqlcgen.ResolveCurrentApplicationNestingNodeRow,
) (currentApplicationNestingNode, error) {
	if row.ApplicationVersionID <= 0 || row.ApplicationID <= 0 ||
		!json.Valid([]byte(row.SkillsJson)) || !json.Valid([]byte(row.ChildApplicationsJson)) {
		return currentApplicationNestingNode{}, errInvalidCurrentApplicationNesting
	}
	var skills []currentApplicationNestingSkill
	if err := decodeCurrentApplicationNestingJSON([]byte(row.SkillsJson), &skills); err != nil || skills == nil {
		return currentApplicationNestingNode{}, errInvalidCurrentApplicationNesting
	}
	seenSkills := make(map[int32]struct{}, len(skills))
	for index := range skills {
		skill := &skills[index]
		skill.Name = strings.TrimSpace(skill.Name)
		icon := bytes.TrimSpace(skill.IconMeta)
		if skill.SkillID <= 0 || skill.Name == "" || !utf8.ValidString(skill.Name) ||
			utf8.RuneCountInString(skill.Name) > currentMaxNestedSkillNameRunes ||
			strings.ContainsRune(skill.Name, '\x00') || len(icon) == 0 || !json.Valid(icon) ||
			(!bytes.Equal(icon, []byte("null")) && icon[0] != '{') {
			return currentApplicationNestingNode{}, errInvalidCurrentApplicationNesting
		}
		if _, duplicate := seenSkills[skill.SkillID]; duplicate {
			return currentApplicationNestingNode{}, errInvalidCurrentApplicationNesting
		}
		seenSkills[skill.SkillID] = struct{}{}
		skill.IconMeta = bytes.Clone(icon)
	}
	var children []currentApplicationNestingReference
	if err := decodeCurrentApplicationNestingJSON([]byte(row.ChildApplicationsJson), &children); err != nil || children == nil {
		return currentApplicationNestingNode{}, errInvalidCurrentApplicationNesting
	}
	return currentApplicationNestingNode{
		versionID:     row.ApplicationVersionID,
		applicationID: row.ApplicationID,
		agentType:     row.AgentType,
		skills:        skills,
		children:      children,
	}, nil
}

func decodeCurrentApplicationNestingJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errInvalidCurrentApplicationNesting
	}
	return nil
}

func cloneCurrentApplicationNestingSkills(
	skills []currentApplicationNestingSkill,
) []currentApplicationNestingSkill {
	result := make([]currentApplicationNestingSkill, len(skills))
	for index, skill := range skills {
		result[index] = skill
		result[index].IconMeta = bytes.Clone(skill.IconMeta)
	}
	return result
}

func positiveCurrentApplicationNestingInteger(raw json.RawMessage) (int32, bool) {
	var value int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value <= 0 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}

func currentApplicationAgentTierContribution(agentType string) int {
	if agentType == "pipeline" {
		return 0
	}
	return 1
}

func filterCurrentAdhocApplicationNesting(
	ctx context.Context,
	queries currentApplicationNestingQuerier,
	tools json.RawMessage,
) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(tools))
	decoder.UseNumber()
	var items []any
	if err := decoder.Decode(&items); err != nil || items == nil {
		return nil, errInvalidCurrentApplicationNesting
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errInvalidCurrentApplicationNesting
	}

	filtered := make([]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			return nil, errInvalidCurrentApplicationNesting
		}
		toolType, _ := tool["type"].(string)
		if toolType != "application" {
			filtered = append(filtered, tool)
			continue
		}
		materialized, materializeErr := materializeCurrentApplicationToolNestedSkills(
			ctx,
			queries,
			tool,
			2,
		)
		if materializeErr == nil {
			filtered = append(filtered, materialized)
			continue
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if !errors.Is(materializeErr, errInvalidCurrentApplicationNesting) {
			return nil, materializeErr
		}
	}

	encoded, err := json.Marshal(filtered)
	if err != nil || !json.Valid(encoded) {
		return nil, errInvalidCurrentApplicationNesting
	}
	return encoded, nil
}

func materializeCurrentApplicationVersionNestedSkills(
	ctx context.Context,
	queries currentApplicationNestingQuerier,
	versionDetails json.RawMessage,
) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(versionDetails))
	decoder.UseNumber()
	var version map[string]any
	if err := decoder.Decode(&version); err != nil || version == nil {
		return nil, errInvalidCurrentApplicationNesting
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errInvalidCurrentApplicationNesting
	}
	tools, ok := version["tools"].([]any)
	if !ok {
		return nil, errInvalidCurrentApplicationNesting
	}
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, errInvalidCurrentApplicationNesting
		}
		if tool["type"] != "application" {
			continue
		}
		materialized, err := materializeCurrentApplicationToolNestedSkills(
			ctx,
			queries,
			tool,
			2,
		)
		if err != nil {
			return nil, err
		}
		tools[index] = materialized
	}
	version["tools"] = tools
	encoded, err := json.Marshal(version)
	if err != nil || !json.Valid(encoded) {
		return nil, errInvalidCurrentApplicationNesting
	}
	return encoded, nil
}

func materializeCurrentApplicationToolNestedSkills(
	ctx context.Context,
	queries currentApplicationNestingQuerier,
	tool map[string]any,
	startDepth int,
) (map[string]any, error) {
	settings, ok := tool["settings"].(map[string]any)
	if !ok {
		return nil, errInvalidCurrentApplicationNesting
	}
	applicationID, validApplicationID := positiveCurrentApplicationNestingValue(
		settings["application_id"],
	)
	versionID, validVersionID := positiveCurrentApplicationNestingValue(
		settings["application_version_id"],
	)
	name, validName := tool["name"].(string)
	name = strings.TrimSpace(name)
	if !validApplicationID || !validVersionID || !validName || name == "" ||
		!utf8.ValidString(name) || utf8.RuneCountInString(name) > currentMaxNestedSkillNameRunes ||
		strings.ContainsRune(name, '\x00') {
		return nil, errInvalidCurrentApplicationNesting
	}
	resolution, err := resolveCurrentApplicationNesting(
		ctx,
		queries,
		versionID,
		startDepth,
	)
	if err != nil {
		return nil, err
	}
	if resolution.root.applicationID != applicationID {
		return nil, fmt.Errorf(
			"%w: application version %d belongs to application %d, not %d",
			errInvalidCurrentApplicationNesting,
			versionID,
			resolution.root.applicationID,
			applicationID,
		)
	}
	registry := make([]currentApplicationSkillRegistry, 0, len(resolution.registries)+1)
	if len(resolution.root.skills) > 0 {
		registry = append(registry, currentApplicationSkillRegistry{
			ApplicationID: applicationID, ApplicationVersionID: versionID,
			ApplicationName: name,
			Skills:          cloneCurrentApplicationNestingSkills(resolution.root.skills),
		})
	}
	registry = append(registry, resolution.registries...)
	result := make(map[string]any, len(tool)+1)
	for key, value := range tool {
		result[key] = value
	}
	if len(registry) == 0 {
		delete(result, "nested_skill_registry")
	} else {
		result["nested_skill_registry"] = registry
	}
	return result, nil
}

func positiveCurrentApplicationNestingValue(value any) (int32, bool) {
	var parsed int64
	switch typed := value.(type) {
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		parsed = integer
	case int32:
		parsed = int64(typed)
	case int64:
		parsed = typed
	case int:
		parsed = int64(typed)
	default:
		return 0, false
	}
	if parsed <= 0 || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}
