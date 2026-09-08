package applications

import (
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

type Author struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type Application struct {
	ID string `json:"id"`
	// UUID is the applications.uuid column. It is a second, stable identity
	// for the row; ID is the SERIAL primary key every other endpoint and the
	// UI's /agents/$tab/$agentId route address the application by.
	UUID        string `json:"uuid,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Icon        string `json:"icon,omitempty"`
	// Tags are the names of every tag on every version of the application,
	// deduplicated and sorted. List fills them; the wire always carries the
	// key, and an application with no tags carries `[]` (#841). Get and
	// Create build their own response maps and do not use this field.
	Tags      []string       `json:"tags"`
	FolderID  string         `json:"folder_id,omitempty"`
	Status    string         `json:"status,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at,omitempty"`
	// CreatedBy stays empty for an application. The table has no creator
	// column: `owner_id` is the project. The list path leaves it empty as
	// well, so an empty value is what every read of this type answers with.
	CreatedBy string `json:"created_by,omitempty"`
	// OwnerID is the owning PROJECT of the application, as
	// `applications.owner_id` holds it (#533). It is not the creator. The
	// creator of a version is Version.AuthorID.
	OwnerID      string         `json:"owner_id"`
	Authors      []Author       `json:"authors,omitempty"`
	IsForked     bool           `json:"is_forked"`
	Meta         map[string]any `json:"meta"`
	HasInterrupt bool           `json:"has_interrupt"`
	AgentType    string         `json:"agent_type,omitempty"`
	// Versions carries the versions created alongside the application by
	// Create (CreateRequest.InitialVersion). Read paths populate versions
	// through ListVersions/GetVersion instead.
	Versions []Version `json:"versions,omitempty"`
}

type Version struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	// Config is a DERIVED, READ-ONLY projection of the columns that actually
	// exist (see VersionConfig). Repository writes reject a non-zero Config;
	// set the column-backed fields below instead.
	Config    VersionConfig `json:"config"`
	IsDefault bool          `json:"is_default"`
	Status    string        `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`

	// Column-backed fields — these are the write surface, one per
	// application_versions column (migrations/001_initial.sql).
	AuthorID             int64          `json:"author_id,omitempty"`
	AgentType            string         `json:"agent_type,omitempty"`
	Instructions         string         `json:"instructions,omitempty"`
	WelcomeMessage       string         `json:"welcome_message,omitempty"`
	LLMSettings          map[string]any `json:"llm_settings,omitempty"`
	ConversationStarters []any          `json:"conversation_starters,omitempty"`
	Meta                 map[string]any `json:"meta,omitempty"`
	// PipelineSettings is the pipeline flow-graph layout ({nodes, edges,
	// orientation, layout_version}) stored verbatim in the
	// application_versions.pipeline_settings jsonb column. Nil means "the
	// caller did not send the key"; the repository then leaves the stored
	// value alone rather than blanking it.
	PipelineSettings map[string]any `json:"pipeline_settings,omitempty"`

	// Present names the string-valued columns above that the caller
	// EXPLICITLY sent. The map-valued and slice-valued fields carry their own
	// absent marker — nil is "not sent" and an empty map/slice is a real
	// value — but a string cannot: "" is both "the caller cleared this" and
	// "the caller said nothing". Without this flag UpdateVersion read "" as
	// absence, so a client that cleared the welcome message got a 201 and
	// read the old text back (#824).
	//
	// It is a separate flag rather than a *string on each field because the
	// same struct is the READ surface: every caller of GetVersion /
	// ListVersions dereferences Name and Instructions directly, and the
	// scanner writes them from the row. Only the write paths set Present.
	// It is not part of the wire shape — the HTTP layer decodes presence
	// from the request body and sets it here.
	Present VersionFieldSet `json:"-"`

	// CopySkillsFromVersionID is a create-version option, not stored metadata.
	// Zero disables copying. The repository requires the same application and tenant.
	CopySkillsFromVersionID int32 `json:"-"`
}

// VersionFieldSet marks which string-valued columns a version write carries.
//
// A false flag with a non-empty value still writes the column, so a caller
// that only fills the value keeps working. A true flag with an empty value
// CLEARS the column, which is the whole point of the type.
//
// Whether an empty value is ALLOWED is the caller's policy, not the
// repository's: the HTTP layer refuses an explicit empty `name` or
// `agent_type` (both NOT NULL, both given a default when the create path sees
// "") and accepts an explicit empty `instructions` or `welcome_message`.
type VersionFieldSet struct {
	Name           bool
	AgentType      bool
	Instructions   bool
	WelcomeMessage bool
}

// VersionConfig is a derived projection over the application_versions columns
// that can carry it, not a storage shape of its own:
//
//	Model        <-> llm_settings->>'model_name'
//	Temperature  <-> llm_settings->'temperature'
//	MaxTokens    <-> llm_settings->'max_tokens'
//	SystemPrompt <-> instructions
//
// Tools, Skills, Datasources and Guardrails have NO storage: tools and skills
// live in the entity_tool_mapping / entity_skill_mapping association tables
// this repository does not own, and datasources/guardrails have no column and
// no table anywhere in the tenant schema. Rather than accept and silently drop
// them, the repository rejects a write that sets any field of this struct.
type VersionConfig struct {
	Model        string         `json:"model,omitempty"`
	Temperature  float64        `json:"temperature,omitempty"`
	MaxTokens    int            `json:"max_tokens,omitempty"`
	SystemPrompt string         `json:"system_prompt,omitempty"`
	Tools        []ToolRef      `json:"tools,omitempty"`
	Skills       []SkillRef     `json:"skills,omitempty"`
	Datasources  []string       `json:"datasources,omitempty"`
	Guardrails   *GuardrailsCfg `json:"guardrails,omitempty"`
}

type ToolRef struct {
	ToolkitID string `json:"toolkit_id"`
	ToolName  string `json:"tool_name"`
}

type SkillRef struct {
	SkillID   string `json:"skill_id"`
	VersionID string `json:"version_id,omitempty"`
}

type GuardrailsCfg struct {
	Input  []GuardrailRule `json:"input,omitempty"`
	Output []GuardrailRule `json:"output,omitempty"`
}

type GuardrailRule struct {
	Type   string         `json:"type"`
	Config map[string]any `json:"config,omitempty"`
}

type ListRequest struct {
	ProjectID  string `json:"-"`
	Page       int    `json:"page,omitempty"`
	PageSize   int    `json:"page_size,omitempty"`
	Search     string `json:"search,omitempty"`
	Tags       string `json:"tags,omitempty"`
	FolderID   string `json:"folder_id,omitempty"`
	AgentsType string `json:"-"`
}

type ListResponse struct {
	Rows       []Application `json:"rows"`
	Total      int           `json:"total"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	TotalPages int           `json:"total_pages"`
}

type CreateRequest struct {
	ProjectID   string   `json:"-"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Icon        string   `json:"icon,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	FolderID    string   `json:"folder_id,omitempty"`
	// AuthorID is the authenticated principal's owning auth_core__user id. It
	// is never decoded from the request body — the transport layer sets it
	// from the request context (auth.User.OwningUserID).
	//
	// It was called OwnerID, and the repository wrote it into
	// `applications.owner_id`. That column holds the owning PROJECT (#533), so
	// the name promised the wrong column and the writer stored the wrong kind
	// of number. The value is the AUTHOR of the first version, and the type
	// says which kind of number it is.
	AuthorID ownership.UserID `json:"-"`
	// InitialVersion, when set, is created in the SAME transaction as the
	// application row. An application with no version row is invisible to
	// List (which INNER JOINs application_versions), so a create that only
	// inserts the parent row does not round-trip.
	InitialVersion *Version       `json:"-"`
	Config         *VersionConfig `json:"config,omitempty"`
}

type UpdateRequest struct {
	ProjectID     string   `json:"-"`
	ApplicationID string   `json:"-"`
	Name          *string  `json:"name,omitempty"`
	Description   *string  `json:"description,omitempty"`
	Icon          *string  `json:"icon,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	FolderID      *string  `json:"folder_id,omitempty"`
}

type PredictRequest struct {
	ProjectID string         `json:"-"`
	VersionID string         `json:"-"`
	Input     string         `json:"input"`
	Variables map[string]any `json:"variables,omitempty"`
	Stream    bool           `json:"stream,omitempty"`
}

type PredictResponse struct {
	MessageGroupUID string `json:"message_group_uid"`
	Content         string `json:"content,omitempty"`
	IsStreaming     bool   `json:"is_streaming"`
}
