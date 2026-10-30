// Package connectorapproval is the default-deny catalogue mode and the
// connector approval queue (ADR-0044 decision 1, #1501).
//
// # The mode
//
// The platform's `catalogue` section holds `mode: open | approved_only`.
// `open` (the default) is the behaviour before this package: the toolkit
// type policy and the guardrails decide. `approved_only` makes three kinds
// of connector usable only by the projects an APPROVED connector request
// names:
//
//   - a toolkit type (`jira`, `github`, ...);
//   - a pre-built MCP catalogue entry (toolkit type `mcp_<key>`);
//   - a custom MCP server URL (toolkit type `mcp`, setting `url`).
//
// The governed credential profile (ADR-0039) forces `approved_only`:
// EffectiveMode. A governed run (its worker command carries a security
// context) is held to `approved_only` too, whatever the platform says.
//
// Platform-internal toolkits that reach no external system (nested agents,
// artifacts, memory, vector stores, the built-in MCP categories) are not
// connectors and are never gated: InternalToolkitType.
//
// # Where it is enforced
//
// One View answers every enforcement point, so they cannot disagree: the
// toolkit type catalogue (listing for selection), the toolkit create,
// update and fork paths, MCP discovery (Load Tools), the agent tool freeze,
// and the preparation of every worker command (CommandGate), which catches
// a run frozen before the mode switched.
//
// # Pinning
//
// A custom MCP server's approved tools are pinned by name and digest
// (ToolDigest: SHA-256 of the canonical JSON of the tool's name,
// description and input schema). A tool the server later adds, or changes,
// matches no approved pin and is hidden: filtered out of discovery, left out
// of the frozen run (the worker filters the server's live listing by the
// `pinned_tools` setting the freeze writes), and refused on a direct call.
package connectorapproval

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// ApprovePermission approves or rejects connector requests (administration
// mode; shared 0207 grants it to super_admin and admin).
const ApprovePermission = "connector.approve"

// RequestPermission creates, submits and withdraws a project's connector
// requests: the permission that already gates adding a toolkit.
const RequestPermission = "models.applications.tools.create"

// TargetKind is what a request makes usable.
type TargetKind string

const (
	TargetToolkitType TargetKind = "toolkit_type"
	TargetPrebuiltMCP TargetKind = "prebuilt_mcp"
	TargetCustomMCP   TargetKind = "custom_mcp"
)

// Valid reports whether k is a known kind.
func (k TargetKind) Valid() bool {
	switch k {
	case TargetToolkitType, TargetPrebuiltMCP, TargetCustomMCP:
		return true
	}
	return false
}

// State is a request's state.
type State string

const (
	StateDraft     State = "draft"
	StateSubmitted State = "submitted"
	StateApproved  State = "approved"
	StateRejected  State = "rejected"
	StateWithdrawn State = "withdrawn"
)

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StateDraft, StateSubmitted, StateApproved, StateRejected, StateWithdrawn:
		return true
	}
	return false
}

// Origin is how a request came to exist.
type Origin string

const (
	// OriginUser is a project member's request (RequestedBy is set).
	OriginUser Origin = "user"
	// OriginInventory is an administrator's "Create requests for connectors
	// in use": the on-ramp to approved_only. RequestedBy is zero and
	// CreatedBy is the administrator.
	OriginInventory Origin = "inventory"
)

// ToolCategory is an ADR-0041 tool category: the one definition in
// domain/toolcategory.
type ToolCategory = toolcategory.Category

const (
	CategoryRead          = toolcategory.Read
	CategoryWrite         = toolcategory.Write
	CategoryDelete        = toolcategory.Delete
	CategoryExternalSend  = toolcategory.ExternalSend
	CategoryPayment       = toolcategory.Payment
	CategoryCodeExecution = toolcategory.CodeExecution
)

// DeclaredTool is one tool a request declares.
type DeclaredTool struct {
	Name     string       `json:"name"`
	Category ToolCategory `json:"category"`
	// Description and InputSchema are what the requester saw (a custom MCP
	// server's discovery); the approver reviews them, and Digest is computed
	// from them by the server, never taken from the caller.
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

// Request is one connector request.
type Request struct {
	ID            int64
	ProjectID     int64
	TargetKind    TargetKind
	Target        string
	ToolkitType   string
	PlatformWide  bool
	ProjectIDs    []int64
	Tools         []DeclaredTool
	EgressHosts   []string
	DataClasses   []string
	Justification string
	State         State
	Origin        Origin
	// RequestedBy is the requesting member; zero for an inventory request.
	RequestedBy int64
	// CreatedBy is the administrator who created an inventory request.
	CreatedBy      int64
	DecidedBy      int64
	DecisionReason string
	AttestationID  int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SubmittedAt    *time.Time
	DecidedAt      *time.Time
}

// Draft is the editable part of a request.
type Draft struct {
	TargetKind    TargetKind
	Target        string
	PlatformWide  bool
	ProjectIDs    []int64
	Tools         []DeclaredTool
	EgressHosts   []string
	DataClasses   []string
	Justification string
}

// Bounds, matching the migration's CHECKs.
const (
	MaxTargetBytes        = 2048
	MaxProjects           = 64
	MaxTools              = 256
	MaxEgressHosts        = 64
	MaxDataClasses        = 32
	MaxJustification      = 4000
	MaxDecisionReason     = 1000
	MaxToolNameBytes      = 128
	MaxToolDescription    = 4096
	MaxToolSchemaBytes    = 64 << 10
	maxToolkitTypeBytes   = 256
	customMCPToolkitType  = "mcp"
	prebuiltToolkitPrefix = mcpregistry.PrebuiltToolkitTypePrefix
)

var (
	dataClassPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	toolNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
)

// ValidationError is a request the package refuses to store (400).
type ValidationError struct{ Message string }

func (e ValidationError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return ValidationError{Message: fmt.Sprintf(format, args...)}
}

// InternalToolkitType reports whether a toolkit type is platform-internal:
// it reaches no external system, so it is not a connector and is never
// gated.
func InternalToolkitType(toolkitType string) bool {
	switch toolkitType {
	case "application", "artifact", "memory", "vectorstore", "datasource":
		return true
	}
	_, internal := mcpregistry.InternalBuilderForType(toolkitType)
	return internal
}

// NormalizeTarget validates a target and answers its stored spelling and
// the toolkit type it makes usable.
//
//   - toolkit_type: the type, byte for byte (toolkitcatalogue's rule). `mcp`
//     and `mcp_*` are refused: they are MCP targets.
//   - prebuilt_mcp: the catalogue key (mcpregistry.NormalizeCatalogueKey);
//     the type is `mcp_<key>`.
//   - custom_mcp: the canonical URL (CanonicalMCPURL); the type is `mcp`.
func NormalizeTarget(kind TargetKind, target string) (string, string, error) {
	target = strings.TrimSpace(target)
	if target == "" || len(target) > MaxTargetBytes || !utf8.ValidString(target) {
		return "", "", invalid("a target is required (at most %d bytes)", MaxTargetBytes)
	}
	switch kind {
	case TargetToolkitType:
		toolkitType, err := toolkitcatalogue.ValidateToolkitType(target)
		if err != nil {
			return "", "", invalid("%v", err)
		}
		if toolkitType == customMCPToolkitType || strings.HasPrefix(toolkitType, prebuiltToolkitPrefix) {
			return "", "", invalid("%q is an MCP toolkit: request it as prebuilt_mcp or custom_mcp", toolkitType)
		}
		if InternalToolkitType(toolkitType) {
			return "", "", invalid("%q is platform-internal and needs no approval", toolkitType)
		}
		return toolkitType, toolkitType, nil
	case TargetPrebuiltMCP:
		key := mcpregistry.NormalizeCatalogueKey(target)
		if key == "" || len(prebuiltToolkitPrefix+key) > maxToolkitTypeBytes {
			return "", "", invalid("a pre-built MCP catalogue name is required")
		}
		return key, prebuiltToolkitPrefix + key, nil
	case TargetCustomMCP:
		canonical, err := CanonicalMCPURL(target)
		if err != nil {
			return "", "", invalid("%v", err)
		}
		return canonical, customMCPToolkitType, nil
	}
	return "", "", invalid("target_kind must be toolkit_type, prebuilt_mcp or custom_mcp")
}

// CanonicalMCPURL is the one spelling a custom MCP server URL is approved
// and matched by: mcpregistry's endpoint rule (https, or http to a loopback
// host), lower-case scheme and host, the default port dropped, the path
// without a trailing slash, the query kept, no fragment.
func CanonicalMCPURL(raw string) (string, error) {
	endpoint, err := mcpregistry.ValidateEndpointURL(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("the MCP server URL is not valid: %v", err)
	}
	host := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	port := endpoint.Port()
	if (endpoint.Scheme == "https" && port == "443") || (endpoint.Scheme == "http" && port == "80") {
		port = ""
	}
	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport += ":" + port
	}
	canonical := url.URL{Scheme: endpoint.Scheme, Host: hostport, Path: strings.TrimRight(endpoint.Path, "/"),
		RawPath: strings.TrimRight(endpoint.RawPath, "/"), RawQuery: endpoint.RawQuery}
	text := canonical.String()
	if len(text) > MaxTargetBytes {
		return "", fmt.Errorf("the MCP server URL is longer than %d bytes", MaxTargetBytes)
	}
	return text, nil
}

// HostOfTarget answers the egress host a custom MCP target implies (its
// URL's host and non-default port), so an approval admits the server it
// approves even when the requester did not list it.
func HostOfTarget(kind TargetKind, target string) (string, bool) {
	if kind != TargetCustomMCP {
		return "", false
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Hostname() == "" {
		return "", false
	}
	host := parsed.Hostname()
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	entry, err := egresslib.ParseEntry(host)
	if err != nil {
		return "", false
	}
	return entry.String(), true
}

// NormalizeDraft validates a draft and answers its stored form: the target
// normalised, projects deduplicated and sorted, tools validated (a custom MCP
// tool's digest computed from its description and input schema), egress
// hosts in the egresslib grammar, data classes as short identifiers. The
// projects are not checked for existence here (the store does that).
func NormalizeDraft(draft Draft) (Draft, string, error) {
	return normalizeDraft(draft, false)
}

// NormalizeInventoryDraft is NormalizeDraft for an inventory request: a
// custom MCP server whose tools were never loaded may declare none (an
// approval then pins none, so the server is admitted with every tool hidden
// until a member requests its tools).
func NormalizeInventoryDraft(draft Draft) (Draft, string, error) {
	return normalizeDraft(draft, true)
}

// ValidToolName reports whether name is a tool name a request may declare.
func ValidToolName(name string) bool { return toolNamePattern.MatchString(name) }

// Originator is who may not decide a request: the requester, or the creator of
// an inventory request.
func (r Request) Originator() int64 {
	if r.RequestedBy > 0 {
		return r.RequestedBy
	}
	return r.CreatedBy
}

func normalizeDraft(draft Draft, inventory bool) (Draft, string, error) {
	if !draft.TargetKind.Valid() {
		return Draft{}, "", invalid("target_kind must be toolkit_type, prebuilt_mcp or custom_mcp")
	}
	target, toolkitType, err := NormalizeTarget(draft.TargetKind, draft.Target)
	if err != nil {
		return Draft{}, "", err
	}
	draft.Target = target

	projects := make([]int64, 0, len(draft.ProjectIDs))
	seen := map[int64]bool{}
	for _, id := range draft.ProjectIDs {
		if id <= 0 {
			return Draft{}, "", invalid("project ids are positive integers")
		}
		if !seen[id] {
			seen[id] = true
			projects = append(projects, id)
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i] < projects[j] })
	if draft.PlatformWide && len(projects) > 0 {
		return Draft{}, "", invalid("a platform-wide request names no projects")
	}
	if !draft.PlatformWide && len(projects) == 0 {
		return Draft{}, "", invalid("name at least one project, or request the connector platform-wide")
	}
	if len(projects) > MaxProjects {
		return Draft{}, "", invalid("at most %d projects", MaxProjects)
	}
	draft.ProjectIDs = projects

	if len(draft.Tools) > MaxTools {
		return Draft{}, "", invalid("at most %d declared tools", MaxTools)
	}
	tools := make([]DeclaredTool, 0, len(draft.Tools))
	names := map[string]bool{}
	for _, tool := range draft.Tools {
		tool.Name = strings.TrimSpace(tool.Name)
		if !toolNamePattern.MatchString(tool.Name) {
			return Draft{}, "", invalid("tool name %q is not a tool name", truncate(tool.Name, 64))
		}
		if names[strings.ToLower(tool.Name)] {
			return Draft{}, "", invalid("tool %q is declared twice", tool.Name)
		}
		names[strings.ToLower(tool.Name)] = true
		if tool.Category == "" {
			// ADR-0041: an undeclared tool is `write`.
			tool.Category = CategoryWrite
		}
		if !tool.Category.Valid() {
			return Draft{}, "", invalid("tool %q: category must be read, write, delete, external_send, payment or code_execution", tool.Name)
		}
		if len(tool.Description) > MaxToolDescription || !utf8.ValidString(tool.Description) {
			return Draft{}, "", invalid("tool %q: the description is longer than %d bytes", tool.Name, MaxToolDescription)
		}
		tool.Digest = ""
		if draft.TargetKind == TargetCustomMCP {
			if tool.InputSchema == nil {
				return Draft{}, "", invalid("tool %q: a custom MCP server's tool needs the input_schema its discovery returned, so it can be pinned", tool.Name)
			}
			canonical, err := CanonicalJSON(tool.InputSchema)
			if err != nil || len(canonical) > MaxToolSchemaBytes {
				return Draft{}, "", invalid("tool %q: the input_schema is not JSON of at most %d bytes", tool.Name, MaxToolSchemaBytes)
			}
			tool.Digest, err = ToolDigest(tool.Name, tool.Description, tool.InputSchema)
			if err != nil {
				return Draft{}, "", invalid("tool %q: %v", tool.Name, err)
			}
		} else {
			tool.InputSchema = nil
		}
		tools = append(tools, tool)
	}
	if draft.TargetKind == TargetCustomMCP && len(tools) == 0 && !inventory {
		return Draft{}, "", invalid("a custom MCP server request declares the tools to pin (load the server's tools first)")
	}
	draft.Tools = tools

	hosts := make([]string, 0, len(draft.EgressHosts))
	seenHosts := map[string]bool{}
	for _, raw := range draft.EgressHosts {
		entry, err := egresslib.ParseEntry(strings.TrimSpace(raw))
		if err != nil {
			return Draft{}, "", invalid("egress host %q: %v", truncate(raw, 96), err)
		}
		if text := entry.String(); !seenHosts[text] {
			seenHosts[text] = true
			hosts = append(hosts, text)
		}
	}
	if len(hosts) > MaxEgressHosts {
		return Draft{}, "", invalid("at most %d egress hosts", MaxEgressHosts)
	}
	draft.EgressHosts = hosts

	classes := make([]string, 0, len(draft.DataClasses))
	seenClasses := map[string]bool{}
	for _, class := range draft.DataClasses {
		class = strings.ToLower(strings.TrimSpace(class))
		if class == "" || seenClasses[class] {
			continue
		}
		if !dataClassPattern.MatchString(class) {
			return Draft{}, "", invalid("data class %q is not a short identifier (a-z, 0-9, _ . -)", truncate(class, 64))
		}
		seenClasses[class] = true
		classes = append(classes, class)
	}
	if len(classes) > MaxDataClasses {
		return Draft{}, "", invalid("at most %d data classes", MaxDataClasses)
	}
	draft.DataClasses = classes

	draft.Justification = strings.TrimSpace(draft.Justification)
	if draft.Justification == "" {
		return Draft{}, "", invalid("a justification is required: say why the connector is needed")
	}
	if len(draft.Justification) > MaxJustification || !utf8.ValidString(draft.Justification) {
		return Draft{}, "", invalid("the justification is at most %d bytes", MaxJustification)
	}
	return draft, toolkitType, nil
}

// ValidateDecisionReason bounds an approver's reason. A rejection needs one.
func ValidateDecisionReason(reason string, required bool) (string, error) {
	reason = strings.TrimSpace(reason)
	if required && reason == "" {
		return "", invalid("a reason is required")
	}
	if len(reason) > MaxDecisionReason || !utf8.ValidString(reason) {
		return "", invalid("the reason is at most %d bytes", MaxDecisionReason)
	}
	return reason, nil
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "..."
}
