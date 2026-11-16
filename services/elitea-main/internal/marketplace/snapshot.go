package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// AgentSource names the published version a snapshot is taken from. Schema is
// the quoted tenant schema of the source project (tenantschema), and
// VersionID the published clone in it, whose tool and skill mappings the
// publish has already copied.
type AgentSource struct {
	Schema        string
	ProjectID     int64
	ApplicationID int64
	VersionID     int64
}

// maxNesting bounds how deep nested agents are followed, as the publish embed
// does.
const maxNesting = 5

// VersionDefinition is the part of a version the snapshot keeps verbatim.
type VersionDefinition struct {
	AgentType            string          `json:"agent_type"`
	Instructions         string          `json:"instructions"`
	WelcomeMessage       string          `json:"welcome_message"`
	ConversationStarters json.RawMessage `json:"conversation_starters"`
	LLMSettings          json.RawMessage `json:"llm_settings"`
	Meta                 json.RawMessage `json:"meta"`
	PipelineSettings     json.RawMessage `json:"pipeline_settings"`
	Soul                 json.RawMessage `json:"soul"`
	AvatarPack           json.RawMessage `json:"avatar_pack"`
}

// ToolRef is a tool mapping of the snapshot: which requirement backs it and
// which tools of it the agent uses. It holds no setting and no credential.
type ToolRef struct {
	Key           string   `json:"key"`
	SelectedTools []string `json:"selected_tools"`
}

// SkillRef is a skill mapping of the snapshot.
type SkillRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// NestedAgent is an agent the snapshot's agent uses as a tool, kept as an
// embedded definition.
type NestedAgent struct {
	ToolName string         `json:"tool_name"`
	Node     DefinitionNode `json:"node"`
}

// DefinitionNode is the definition of one agent of a snapshot, with the agents
// it uses.
type DefinitionNode struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Icon         *string           `json:"icon"`
	Tags         []string          `json:"tags"`
	Version      VersionDefinition `json:"version"`
	Tools        []ToolRef         `json:"tools"`
	Skills       []SkillRef        `json:"skills"`
	NestedAgents []NestedAgent     `json:"nested_agents"`
}

// ParseDefinition reads a stored definition.
func ParseDefinition(raw json.RawMessage) (DefinitionNode, error) {
	var node DefinitionNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return DefinitionNode{}, fmt.Errorf("marketplace: read the stored definition: %w", err)
	}
	return node, nil
}

type collector struct {
	order []string
	items map[string]*Requirement
}

func newCollector() *collector {
	return &collector{items: map[string]*Requirement{}}
}

func requirementKey(kind, typ, name string) string {
	return kind + ":" + typ + ":" + name
}

func (c *collector) add(kind, typ, name string, tools []string) string {
	key := requirementKey(kind, typ, name)
	item, ok := c.items[key]
	if !ok {
		item = &Requirement{Key: key, Kind: kind, Type: typ, Name: name, Tools: []string{}}
		c.items[key] = item
		c.order = append(c.order, key)
	}
	seen := map[string]bool{}
	for _, tool := range item.Tools {
		seen[tool] = true
	}
	for _, tool := range tools {
		if tool == "" || seen[tool] {
			continue
		}
		seen[tool] = true
		item.Tools = append(item.Tools, tool)
		if IsWriteTool(tool) {
			item.HasWriteTools = true
		}
	}
	sort.Strings(item.Tools)
	return key
}

func (c *collector) list() []Requirement {
	out := make([]Requirement, 0, len(c.order))
	for _, key := range c.order {
		out = append(out, *c.items[key])
	}
	return out
}

// BuildAgentSnapshot reads the published version in the source schema and
// builds the snapshot a listing version stores: the definition, the
// requirements, the tier and the digest.
func BuildAgentSnapshot(ctx context.Context, db DB, src AgentSource) (Snapshot, error) {
	collect := newCollector()
	node, version, err := buildNode(ctx, db, src.Schema, src.ApplicationID, src.VersionID, collect, 0, map[int64]bool{})
	if err != nil {
		return Snapshot{}, err
	}
	tier, err := sourceTier(ctx, db, src.ProjectID, src.ApplicationID)
	if err != nil {
		return Snapshot{}, err
	}
	reqs := Requirements{Items: collect.list(), ModelClass: ModelChat, Tier: tier}
	for _, item := range reqs.Items {
		if item.Kind == RequirementToolkit || item.Kind == RequirementMCP {
			reqs.ModelClass = ModelToolCalling
		}
		if item.HasWriteTools {
			reqs.HasWriteTools = true
		}
	}
	definition, err := json.Marshal(node)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: encode the definition: %w", err)
	}
	digest, err := DigestOf(definition, reqs)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: digest the snapshot: %w", err)
	}
	category := ""
	var icon *string
	if len(version.Meta) > 0 {
		var meta map[string]any
		if json.Unmarshal(version.Meta, &meta) == nil {
			category, _ = meta["category"].(string)
			if iconMeta, ok := meta["icon_meta"].(map[string]any); ok {
				if url, ok := iconMeta["url"].(string); ok && url != "" {
					icon = &url
				}
			}
		}
	}
	return Snapshot{
		Kind: KindAgent, Name: node.Name, Description: node.Description, Icon: icon,
		Category: category, Tags: node.Tags, SoulSummary: soulSummary(version.Soul),
		Tier: tier, Definition: definition, Requirements: reqs, Digest: digest,
	}, nil
}

// sourceTier is the risk tier of the source agent; an agent with no registry
// row is low, as a new agent is.
func sourceTier(ctx context.Context, db DB, projectID, applicationID int64) (string, error) {
	var tier string
	err := db.QueryRow(ctx, `
SELECT risk_tier FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return "low", nil
	}
	if err != nil {
		return "", fmt.Errorf("marketplace: read the source tier: %w", err)
	}
	if TierRank(tier) < 0 {
		return "low", nil
	}
	return tier, nil
}

func rawOrNull(text *string) json.RawMessage {
	if text == nil || strings.TrimSpace(*text) == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(*text)
}

func soulSummary(soul json.RawMessage) string {
	if len(soul) == 0 || string(soul) == "null" {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(soul, &doc); err != nil {
		// The column may hold a JSON string of the document.
		var text string
		if json.Unmarshal(soul, &text) != nil || json.Unmarshal([]byte(text), &doc) != nil {
			return ""
		}
	}
	role, _ := doc["role"].(string)
	body, _ := doc["body"].(string)
	body = strings.TrimSpace(body)
	if len([]rune(body)) > 300 {
		body = string([]rune(body)[:300])
	}
	return strings.TrimSpace(role + " " + body)
}

func buildNode(ctx context.Context, db DB, schema string, applicationID, versionID int64, collect *collector, depth int, visiting map[int64]bool) (DefinitionNode, VersionDefinition, error) {
	if visiting[versionID] {
		return DefinitionNode{}, VersionDefinition{}, fmt.Errorf("marketplace: version %d uses itself as a tool", versionID)
	}
	visiting[versionID] = true
	defer delete(visiting, versionID)

	node := DefinitionNode{Tags: []string{}, Tools: []ToolRef{}, Skills: []SkillRef{}, NestedAgents: []NestedAgent{}}
	if err := db.QueryRow(ctx, fmt.Sprintf(
		`SELECT name, COALESCE(description, ''), icon FROM %s.applications WHERE id = $1`, schema), applicationID).
		Scan(&node.Name, &node.Description, &node.Icon); err != nil {
		return DefinitionNode{}, VersionDefinition{}, fmt.Errorf("marketplace: read application %d: %w", applicationID, err)
	}
	var starters, llm, meta, pipeline, soul, avatar *string
	var version VersionDefinition
	if err := db.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(agent_type, ''), COALESCE(instructions, ''), COALESCE(welcome_message, ''),
       conversation_starters::text, llm_settings::text, meta::text, pipeline_settings::text, soul::text, avatar_pack::text
FROM %s.application_versions WHERE id = $1`, schema), versionID).
		Scan(&version.AgentType, &version.Instructions, &version.WelcomeMessage, &starters, &llm, &meta, &pipeline, &soul, &avatar); err != nil {
		return DefinitionNode{}, VersionDefinition{}, fmt.Errorf("marketplace: read version %d: %w", versionID, err)
	}
	version.ConversationStarters = rawOrNull(starters)
	version.LLMSettings = rawOrNull(llm)
	version.Meta = cleanMeta(rawOrNull(meta))
	version.PipelineSettings = rawOrNull(pipeline)
	version.Soul = rawOrNull(soul)
	version.AvatarPack = rawOrNull(avatar)
	node.Version = version

	tagRows, err := db.Query(ctx, fmt.Sprintf(`
SELECT t.name FROM %s.application_version_tag_association a JOIN %s.tags t ON t.id = a.tag_id
WHERE a.version_id = $1 ORDER BY t.name`, schema, schema), versionID)
	if err != nil {
		return DefinitionNode{}, VersionDefinition{}, fmt.Errorf("marketplace: read the tags: %w", err)
	}
	for tagRows.Next() {
		var tag string
		if err := tagRows.Scan(&tag); err != nil {
			tagRows.Close()
			return DefinitionNode{}, VersionDefinition{}, err
		}
		node.Tags = append(node.Tags, tag)
	}
	tagRows.Close()
	if err := tagRows.Err(); err != nil {
		return DefinitionNode{}, VersionDefinition{}, err
	}

	type toolRow struct {
		name, typ, selected, childApp, childVersion string
	}
	var tools []toolRow
	toolRows, err := db.Query(ctx, fmt.Sprintf(`
SELECT t.name, t.type, COALESCE(m.selected_tools::text, '[]'),
       COALESCE(t.settings->>'application_id', ''), COALESCE(t.settings->>'application_version_id', '')
FROM %s.entity_tool_mapping m JOIN %s.elitea_tools t ON t.id = m.tool_id
WHERE m.entity_version_id = $1 ORDER BY m.id`, schema, schema), versionID)
	if err != nil {
		return DefinitionNode{}, VersionDefinition{}, fmt.Errorf("marketplace: read the tool mappings: %w", err)
	}
	for toolRows.Next() {
		var row toolRow
		if err := toolRows.Scan(&row.name, &row.typ, &row.selected, &row.childApp, &row.childVersion); err != nil {
			toolRows.Close()
			return DefinitionNode{}, VersionDefinition{}, err
		}
		tools = append(tools, row)
	}
	toolRows.Close()
	if err := toolRows.Err(); err != nil {
		return DefinitionNode{}, VersionDefinition{}, err
	}
	// A published version holds two references for each sub-agent: the
	// author's own child, copied with the mappings, and the embedded copy the
	// publish made. The snapshot keeps one per tool name, the embedded copy
	// when there is one.
	type nestedPick struct {
		name                string
		childApp, childVers int64
		embedded            bool
	}
	var picks []nestedPick
	pickAt := map[string]int{}
	for _, row := range tools {
		if row.typ != "application" {
			continue
		}
		childApp, appErr := strconv.ParseInt(row.childApp, 10, 64)
		childVersion, versionErr := strconv.ParseInt(row.childVersion, 10, 64)
		if appErr != nil || versionErr != nil || depth >= maxNesting {
			continue
		}
		var status string
		if err := db.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(status, '') FROM %s.application_versions WHERE id = $1`, schema), childVersion).Scan(&status); err != nil {
			continue
		}
		pick := nestedPick{name: row.name, childApp: childApp, childVers: childVersion, embedded: status == "embedded"}
		if at, seen := pickAt[row.name]; seen {
			if pick.embedded && !picks[at].embedded {
				picks[at] = pick
			}
			continue
		}
		pickAt[row.name] = len(picks)
		picks = append(picks, pick)
	}
	for _, pick := range picks {
		child, _, err := buildNode(ctx, db, schema, pick.childApp, pick.childVers, collect, depth+1, visiting)
		if err != nil {
			return DefinitionNode{}, VersionDefinition{}, err
		}
		collect.add(RequirementAgent, "", pick.name, nil)
		node.NestedAgents = append(node.NestedAgents, NestedAgent{ToolName: pick.name, Node: child})
	}
	for _, row := range tools {
		if row.typ == "application" {
			continue
		}
		var selected []string
		_ = json.Unmarshal([]byte(row.selected), &selected) // a malformed column reads as no tools
		kind := RequirementToolkit
		if row.typ == "mcp" {
			kind = RequirementMCP
		}
		key := collect.add(kind, row.typ, row.name, selected)
		if selected == nil {
			selected = []string{}
		}
		node.Tools = append(node.Tools, ToolRef{Key: key, SelectedTools: selected})
	}

	skillRows, err := db.Query(ctx, fmt.Sprintf(`
SELECT s.name FROM %s.entity_skill_mapping m JOIN %s.skills s ON s.id = m.skill_id
WHERE m.entity_version_id = $1 ORDER BY m.id`, schema, schema), versionID)
	if err != nil {
		return DefinitionNode{}, VersionDefinition{}, fmt.Errorf("marketplace: read the skill mappings: %w", err)
	}
	var skillNames []string
	for skillRows.Next() {
		var name string
		if err := skillRows.Scan(&name); err != nil {
			skillRows.Close()
			return DefinitionNode{}, VersionDefinition{}, err
		}
		skillNames = append(skillNames, name)
	}
	skillRows.Close()
	if err := skillRows.Err(); err != nil {
		return DefinitionNode{}, VersionDefinition{}, err
	}
	for _, name := range skillNames {
		key := collect.add(RequirementSkill, "", name, nil)
		node.Skills = append(node.Skills, SkillRef{Key: key, Name: name})
	}
	return node, version, nil
}

// cleanMeta removes the keys that name the source: a snapshot is a copy of the
// definition, and the install writes its own origin keys.
func cleanMeta(raw json.RawMessage) json.RawMessage {
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil || meta == nil {
		return json.RawMessage("{}")
	}
	for _, key := range []string{
		"source_version_id", "source_project_id", "source_application_id", "published_by", "status",
		"parent_entity_id", "parent_project_id", "parent_author_id", "parent_published_app_id",
		"parent_published_version_id", MetaOriginKind, MetaOriginListingID, MetaOriginVersionID,
		MetaOriginDigest, MetaOriginVersionName, MetaOriginListingName, "release_notes",
	} {
		delete(meta, key)
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// SkillSource names a published skill version in the source schema.
type SkillSource struct {
	Schema    string
	ProjectID int64
	SkillID   int64
	VersionID int64
}

// BuildSkillSnapshot reads a published skill version. A skill has no tools, so
// its requirements are empty; the definition holds its instructions and tags.
func BuildSkillSnapshot(ctx context.Context, db DB, src SkillSource) (Snapshot, error) {
	var name, description, instructions string
	if err := db.QueryRow(ctx, fmt.Sprintf(
		`SELECT name, COALESCE(description, '') FROM %s.skills WHERE id = $1`, src.Schema), src.SkillID).
		Scan(&name, &description); err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: read skill %d: %w", src.SkillID, err)
	}
	var meta *string
	if err := db.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(instructions, ''), meta::text FROM %s.skill_versions WHERE id = $1`, src.Schema), src.VersionID).
		Scan(&instructions, &meta); err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: read skill version %d: %w", src.VersionID, err)
	}
	tags := []string{}
	tagRows, err := db.Query(ctx, fmt.Sprintf(`
SELECT t.name FROM %s.skill_version_tag_association a JOIN %s.tags t ON t.id = a.tag_id
WHERE a.version_id = $1 ORDER BY t.name`, src.Schema, src.Schema), src.VersionID)
	if err == nil {
		for tagRows.Next() {
			var tag string
			if tagRows.Scan(&tag) == nil {
				tags = append(tags, tag)
			}
		}
		tagRows.Close()
	}
	definition, err := json.Marshal(map[string]any{
		"name": name, "description": description, "instructions": instructions, "tags": tags,
		"meta": cleanMeta(rawOrNull(meta)),
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: encode the skill definition: %w", err)
	}
	reqs := Requirements{Items: []Requirement{}, ModelClass: ModelChat, Tier: "low"}
	digest, err := DigestOf(definition, reqs)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Kind: KindSkill, Name: name, Description: description, Tags: tags, Tier: "low",
		Definition: definition, Requirements: reqs, Digest: digest,
	}, nil
}
