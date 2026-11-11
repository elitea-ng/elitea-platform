// Package agentproposal is ADR-0050 decision 5a (issue 1615): an agent
// proposes that a temporary sub-agent it started become a saved agent, and a
// person decides.
//
// WHAT A PROPOSAL IS. A row of elitea_lifecycle.agent_proposals (shared/0234)
// with a draft the review form opens with, the evidence (the spawn it
// promotes, by its ephemeral_agents key) and suggested memory seeds. It is
// not an agent: Create calls the ordinary create route, and only that writes
// an applications row. The draft's tools are references the person accepts
// or drops, never a grant: the run grants of the child are evidence.
//
// WHO MAY PROPOSE. The proposing agent's version sets
// meta.capabilities.can_propose_agents (CapabilityKey, an ADR-0040 change at
// the agent's tier), and the person the run served holds
// models.applications.applications.create in the project. Both are checked by
// Main at the tool call; the worker only binds the tool when the flag is set.
//
// LIMITS. At most MaxOpenPerAgent open proposals per proposing agent per
// project, and a dismissed spec (its profile digest) cannot be proposed again
// by the same agent in the same project for SuppressFor.
package agentproposal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

const (
	// CapabilitiesKey is the meta key of an agent version's capabilities.
	CapabilitiesKey = "capabilities"
	// CapabilityKey is the capability that binds propose_agent.
	CapabilityKey = "can_propose_agents"
	// CreatePermission is what the person the run served must hold.
	CreatePermission = "models.applications.applications.create"

	// MaxOpenPerAgent bounds the open proposals of one proposing agent in
	// one project.
	MaxOpenPerAgent = 3
	// SuppressFor is how long a dismissed spec cannot be proposed again.
	SuppressFor = 30 * 24 * time.Hour

	MaxRationaleBytes    = 4 * 1024
	MaxInstructionsBytes = 16 * 1024
	MaxSeeds             = 20
	MaxSoulBytes         = 2 * 1024
	MaxDisplayNameChars  = 60
	MaxAvatarBytes       = 128
	MaxModelBytes        = 256
	MaxTools             = 64
	// MaxToolKeyBytes is the runtime's tool key bound (agent-runtime
	// tool_envelope.rs MAX_KEY_BYTES): a key the spawn accepted is never
	// refused here.
	MaxToolKeyBytes = 512
	// MaxDescriptionChars is the agent description bound (AgentDefinition
	// in v2.yaml, tenant applications.description VARCHAR(2304)).
	MaxDescriptionChars = 2304
	// MaxNameChars is the agent name bound of the create route
	// (apps/elitea-web shared/lib/limits MAX_NAME_LENGTH).
	MaxNameChars = 32
)

// Status is a proposal's state.
type Status string

const (
	StatusProposed  Status = "proposed"
	StatusCreated   Status = "created"
	StatusDismissed Status = "dismissed"
)

// Outcome codes the propose_agent tool answers with.
const (
	OutcomeProposed   = "proposed"
	OutcomeDuplicate  = "already_proposed"
	OutcomeLimit      = "proposal_limit"
	OutcomeSuppressed = "proposal_suppressed"
	OutcomeNotAllowed = "not_allowed"
	OutcomeUnknown    = "ephemeral_agent_unknown"
	OutcomeInvalid    = "invalid_arguments"
)

var (
	agentIDPattern = regexp.MustCompile(`^a_[0-9a-f]{8}$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidAgentID reports whether id is a sub-agent id ("a_" and 8 hex digits).
func ValidAgentID(id string) bool { return agentIDPattern.MatchString(id) }

// ValidDigest reports whether digest is a lowercase hex SHA-256.
func ValidDigest(digest string) bool { return digestPattern.MatchString(digest) }

// CanPropose reads meta.capabilities.can_propose_agents of a version's meta.
// Anything but a literal true is false.
func CanPropose(meta map[string]any) bool {
	capabilities, _ := meta[CapabilitiesKey].(map[string]any)
	allowed, _ := capabilities[CapabilityKey].(bool)
	return allowed
}

// CheckCapabilities refuses a meta.capabilities block that is not an object
// of booleans. Absent and null pass.
func CheckCapabilities(meta map[string]any) error {
	raw, present := meta[CapabilitiesKey]
	if !present || raw == nil {
		return nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return errors.New("meta.capabilities must be an object")
	}
	for key, value := range object {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("meta.capabilities.%s must be true or false", key)
		}
	}
	return nil
}

// Tool is one tool the sub-agent was given, as a reference: the key the
// spawn named ("toolkit/tool" or "toolkit/*") and, when the worker knows it,
// the toolkit's type.
type Tool struct {
	Key         string `json:"key"`
	ToolkitType string `json:"toolkit_type,omitempty"`
}

// Profile is the sub-agent's profile as the worker reports it. Its digest is
// ephemeral_agents.profile_digest; Main reads that, never a digest the
// worker names here.
type Profile struct {
	Label       string `json:"label"`
	DisplayName string `json:"display_name"`
	Soul        string `json:"soul"`
	Avatar      string `json:"avatar"`
	Tools       []Tool `json:"tools"`
	// Model is the catalogue model the sub-agent ran on (profile.model,
	// issue 1825); empty for the parent's model. The digest covers it; the
	// draft does not carry it (a saved agent picks its own model).
	Model string `json:"model,omitempty"`
}

// Seed is a suggested memory entry the person may keep.
type Seed struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// Request is the propose_agent tool's body.
type Request struct {
	EphemeralAgentID string `json:"ephemeral_agent_id"`
	Rationale        string `json:"rationale"`
	// Instructions are the ones the proposing agent suggests for the saved
	// agent; the spawn's record never holds its task.
	Instructions string  `json:"instructions"`
	Profile      Profile `json:"profile"`
	MemorySeeds  []Seed  `json:"memory_seeds"`
}

// Error is a request the tool refuses; Code is an Outcome code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(message string) error { return &Error{Code: OutcomeInvalid, Message: message} }

// plain is model-written multi-line text: line breaks (\n, \r\n) and tabs
// are kept, other control characters are refused.
func plain(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	})
}

// storable is text the proposal records verbatim because the runtime's
// profile digest covers it (the soul): any valid UTF-8 the runtime accepted,
// save NUL, which PostgreSQL text cannot hold. Refusing more would refuse a
// spawn the model cannot change, since the profile comes from the ledger.
func storable(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func oneLine(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl)
}

// Check bounds the request and answers it trimmed. The memory seeds go
// through the agent memory content check (the secret scanner included).
func Check(request Request) (Request, error) {
	out := Request{
		EphemeralAgentID: strings.TrimSpace(request.EphemeralAgentID),
		Rationale:        strings.TrimSpace(request.Rationale),
		Instructions:     strings.TrimSpace(request.Instructions),
	}
	if !ValidAgentID(out.EphemeralAgentID) {
		return Request{}, invalid("ephemeral_agent_id must be the id spawn_agent answered (a_ and 8 hex digits).")
	}
	if out.Rationale == "" || len(out.Rationale) > MaxRationaleBytes {
		return Request{}, invalid(fmt.Sprintf("rationale must say why, in at most %d bytes.", MaxRationaleBytes))
	}
	if !plain(out.Rationale) {
		return Request{}, invalid("rationale is plain text: no control characters other than line breaks and tabs.")
	}
	if len(out.Instructions) > MaxInstructionsBytes {
		return Request{}, invalid(fmt.Sprintf("instructions are at most %d bytes.", MaxInstructionsBytes))
	}
	if !plain(out.Instructions) {
		return Request{}, invalid("instructions are plain text: no control characters other than line breaks and tabs.")
	}
	profile := request.Profile
	out.Profile = Profile{
		Label:       strings.TrimSpace(profile.Label),
		DisplayName: strings.TrimSpace(profile.DisplayName),
		Soul:        strings.TrimSpace(profile.Soul),
		Avatar:      strings.TrimSpace(profile.Avatar),
		Model:       strings.TrimSpace(profile.Model),
	}
	for _, text := range []string{out.Profile.Label, out.Profile.DisplayName} {
		if utf8.RuneCountInString(text) > MaxDisplayNameChars || !oneLine(text) {
			return Request{}, invalid("the sub-agent's label and display name are at most 60 characters.")
		}
	}
	if len(out.Profile.Soul) > MaxSoulBytes {
		return Request{}, invalid("the sub-agent's soul is at most 2048 bytes.")
	}
	if !storable(out.Profile.Soul) {
		return Request{}, invalid("the sub-agent's soul is not valid UTF-8 text.")
	}
	if len(out.Profile.Avatar) > MaxAvatarBytes || !oneLine(out.Profile.Avatar) {
		return Request{}, invalid("the sub-agent's avatar is a short key.")
	}
	if len(out.Profile.Model) > MaxModelBytes || !oneLine(out.Profile.Model) || out.Profile.Model == "inherit" {
		return Request{}, invalid("the sub-agent's model is a model name of at most 256 bytes.")
	}
	if len(profile.Tools) > MaxTools {
		return Request{}, invalid(fmt.Sprintf("at most %d tools.", MaxTools))
	}
	if profile.Tools != nil {
		// Absent tools and an empty list are different profiles.
		out.Profile.Tools = make([]Tool, 0, len(profile.Tools))
	}
	seen := map[string]struct{}{}
	for _, tool := range profile.Tools {
		// The runtime's key form (tool_envelope.rs ToolKey): each half
		// trimmed and lower-cased, a wildcard only as the whole tool.
		toolkitType := strings.TrimSpace(tool.ToolkitType)
		toolkit, name, found := strings.Cut(tool.Key, "/")
		toolkit, name = RuntimeLower(strings.TrimSpace(toolkit)), RuntimeLower(strings.TrimSpace(name))
		key := toolkit + "/" + name
		if !found || toolkit == "" || name == "" || strings.Contains(name, "/") ||
			(name != "*" && strings.Contains(name, "*")) || len(tool.Key) > MaxToolKeyBytes ||
			!oneLine(tool.Key) || len(toolkitType) > MaxToolKeyBytes || !oneLine(toolkitType) {
			return Request{}, invalid("each tool is \"toolkit/tool\" or \"toolkit/*\".")
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out.Profile.Tools = append(out.Profile.Tools, Tool{Key: key, ToolkitType: toolkitType})
	}
	if len(request.MemorySeeds) > MaxSeeds {
		return Request{}, invalid(fmt.Sprintf("at most %d memory seeds.", MaxSeeds))
	}
	for _, seed := range request.MemorySeeds {
		kind := agentmemory.Kind(strings.TrimSpace(seed.Kind))
		if kind == "" {
			kind = agentmemory.KindFact
		}
		if !kind.AgentWritable() {
			return Request{}, invalid("a memory seed's kind is fact, preference or episode.")
		}
		content, err := agentmemory.CheckContent(seed.Content)
		if err != nil {
			return Request{}, invalid("a memory seed was refused: " + err.Error())
		}
		out.MemorySeeds = append(out.MemorySeeds, Seed{Kind: string(kind), Content: content})
	}
	return out, nil
}

// ToolCategory is one proposed tool with its ADR-0041 category, for the
// review form.
type ToolCategory struct {
	Key         string `json:"key"`
	ToolkitType string `json:"toolkit_type,omitempty"`
	Category    string `json:"category"`
}

// Categories answers each tool's ADR-0041 category. A wildcard key or an
// undeclared tool is `write` (toolcategory.Default), the safe reading.
func Categories(tools []Tool) []ToolCategory {
	out := make([]ToolCategory, 0, len(tools))
	for _, tool := range tools {
		toolkit, name, _ := strings.Cut(tool.Key, "/")
		toolkitType := tool.ToolkitType
		if toolkitType == "" {
			toolkitType = toolkit
		}
		category := toolcategory.Default
		if name != "*" {
			category = toolcategory.Of(toolkitType, name)
		}
		out = append(out, ToolCategory{Key: tool.Key, ToolkitType: tool.ToolkitType, Category: string(category)})
	}
	return out
}

// Draft builds the AgentDefinition-shaped draft the review form opens with.
// The name comes from the display name, else the label, else the parent
// agent's name; it is cut to the create route's bound. The soul's text
// becomes the soul document's body and its display name the soul's. The
// new agent's tier is never carried: every new agent starts low.
func Draft(request Request) map[string]any {
	name := request.Profile.DisplayName
	if name == "" {
		name = request.Profile.Label
	}
	if name == "" {
		name = "Proposed agent"
	}
	if runes := []rune(name); len(runes) > MaxNameChars {
		name = strings.TrimSpace(string(runes[:MaxNameChars]))
	}
	// The rationale (up to 4 KiB) opens as the description, cut to the
	// agent description bound so the editor can save the agent later.
	description := request.Rationale
	if runes := []rune(description); len(runes) > MaxDescriptionChars {
		description = strings.TrimSpace(string(runes[:MaxDescriptionChars]))
	}
	draft := map[string]any{
		"name":                  name,
		"description":           description,
		"instructions":          request.Instructions,
		"welcome_message":       "",
		"conversation_starters": []any{},
		"risk_tier":             "low",
	}
	if request.Profile.Soul != "" || request.Profile.DisplayName != "" {
		soul := map[string]any{"schema": "elitea.soul/v1"}
		if request.Profile.DisplayName != "" {
			soul["display_name"] = request.Profile.DisplayName
		}
		if request.Profile.Soul != "" {
			soul["body"] = request.Profile.Soul
		}
		draft["soul"] = soul
	}
	tools := make([]any, 0, len(request.Profile.Tools))
	for _, tool := range Categories(request.Profile.Tools) {
		tools = append(tools, map[string]any{"key": tool.Key, "toolkit_type": tool.ToolkitType, "category": tool.Category})
	}
	draft["tools"] = tools
	if request.Profile.Avatar != "" {
		draft["avatar"] = request.Profile.Avatar
	}
	return draft
}

// SpecDigest is the profile's digest exactly as the runtime computes it for
// the spawn's record (libs/rust/agent-runtime subagents/profile.rs
// Profile::digest, stored as ephemeral_agents.profile_digest): the SHA-256 of
// the sorted-key compact JSON of {avatar, display_name, model (the name, or
// "inherit" for the parent's),
// schema "elitea.subagent.profile.v1", soul, tools}, an unset key null, the
// tool keys lower-case, sorted and unique. The label is not part of it.
func SpecDigest(profile Profile) string {
	var tools []string
	if profile.Tools != nil {
		tools = make([]string, 0, len(profile.Tools))
		seen := map[string]struct{}{}
		for _, tool := range profile.Tools {
			key := runtimeToolKey(tool.Key)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			tools = append(tools, key)
		}
		sort.Strings(tools)
	}
	var out strings.Builder
	out.WriteString(`{"avatar":`)
	optionalString(&out, profile.Avatar)
	out.WriteString(`,"display_name":`)
	optionalString(&out, profile.DisplayName)
	out.WriteString(`,"model":`)
	model := profile.Model
	if model == "" {
		model = "inherit"
	}
	quote(&out, model)
	out.WriteString(`,"schema":"elitea.subagent.profile.v1","soul":`)
	optionalString(&out, profile.Soul)
	out.WriteString(`,"tools":`)
	if tools == nil {
		out.WriteString("null")
	} else {
		out.WriteByte('[')
		for index, tool := range tools {
			if index > 0 {
				out.WriteByte(',')
			}
			quote(&out, tool)
		}
		out.WriteByte(']')
	}
	out.WriteByte('}')
	sum := sha256.Sum256([]byte(out.String()))
	return hex.EncodeToString(sum[:])
}

func optionalString(out *strings.Builder, text string) {
	if text == "" {
		out.WriteString("null")
		return
	}
	quote(out, text)
}

// quote writes text as serde_json does: the two short escapes for the quote
// and the backslash, the five short control escapes, \u00xx for the other
// control characters, everything else as is.
func quote(out *strings.Builder, text string) {
	const hexDigits = "0123456789abcdef"
	out.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[r>>4])
				out.WriteByte(hexDigits[r&0xf])
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
}
