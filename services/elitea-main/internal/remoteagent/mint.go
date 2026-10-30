// Package remoteagent authenticates calls between a run and agents outside
// it (ADR-0044 decision 4, #1501).
//
// Outbound (MintService): a worker asks Main, under its live claim, for a
// short-lived delegation assertion for one remote endpoint. Main checks that
// the endpoint is an MCP toolkit admitted in the run, and signs with the
// Elitea-native agent assertion ring. The worker attaches it as the
// Elitea-Agent-Assertion header and keeps any Authorization header as it is.
//
// Inbound (Verifier): the platform's MCP endpoint accepts an assertion only
// from an allowlisted issuer, for its own URL, inside its lifetime, once.
//
// Every decision writes one securityaudit.TypeRemoteAgent event. No event,
// log line or error holds an assertion.
package remoteagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// HeaderName is the HTTP header that carries a delegation assertion. It is
// separate from Authorization, so a server's own credential stays as it is.
const HeaderName = "Elitea-Agent-Assertion"

// Audit actions of TypeRemoteAgent.
const (
	ActionMinted            = "remote_agent.assertion.minted"
	ActionMintRefused       = "remote_agent.assertion.mint_refused"
	ActionAccepted          = "remote_agent.assertion.accepted"
	ActionRefused           = "remote_agent.assertion.refused"
	ActionInvocationRefused = "remote_agent.invocation.refused"
)

// Mint refusals. Each is answered before anything is signed.
var (
	// ErrRunUnreadable: Main could not read the claimed run.
	ErrRunUnreadable = errors.New("remote agent: the run cannot be read")
	// ErrAudienceNotAdmitted: the audience is not the endpoint of an MCP
	// toolkit admitted in the run.
	ErrAudienceNotAdmitted = errors.New("remote agent: the audience is not an endpoint admitted in this run")
	// ErrSignerUnavailable: no agent assertion key ring is configured.
	ErrSignerUnavailable = errors.New("remote agent: no agent assertion key is configured")
	// ErrScopeTooLarge: the run has more toolkit types than the scope claim
	// carries.
	ErrScopeTooLarge = errors.New("remote agent: the run's scope is too large for an assertion")
	// ErrSettingsUnavailable: the remote_agents section could not be read.
	ErrSettingsUnavailable = errors.New("remote agent: the remote agent settings cannot be read")
)

// Run is what Main recorded for one claimed agent run. Every field comes
// from Main's own tables, never from the worker.
type Run struct {
	ExecutionID     string
	Generation      uint64
	TenantProjectID int64
	CapabilityID    string
	// ActorUserID is the user the run was admitted for.
	ActorUserID string
	// ApplicationID is the agent; zero for an ad hoc chat.
	ApplicationID int64
	// Tools is the run's top-level frozen toolkit list (the JSON array the
	// worker materializes from).
	Tools json.RawMessage
	// ToolkitTypes are the canonical toolkit types admitted (sub-agents
	// included), as stored on the execution.
	ToolkitTypes []string
	// Governed is set when the run has a security context.
	Governed bool
	// From the security context, when governed.
	AgentBindingID      string
	MandateID           string
	SubjectPrincipalRef string
}

// RunReader reads a claimed run (repos.RemoteAgentRuns).
type RunReader interface {
	ReadRun(ctx context.Context, executionID string, generation uint64) (Run, error)
}

// NestedTools reads a sub-agent version's frozen toolkit list
// (repos.CurrentNestedApplicationVersionRepository).
type NestedTools interface {
	NestedApplicationTools(ctx context.Context, projectID, applicationID, versionID int64) (json.RawMessage, error)
}

// Signer signs one delegation assertion (*eliteanative.DelegationSigner).
type Signer interface {
	Sign(ctx context.Context, in eliteanative.DelegationRequest) (string, eliteanative.DelegationClaims, error)
}

// Auditor appends one security event in its own transaction
// (repos.RemoteAgentAudit). An error fails the decision.
type Auditor interface {
	RecordRemoteAgent(ctx context.Context, event securityaudit.SecurityEvent) error
}

// Settings reads the remote_agents section.
type Settings func(ctx context.Context) (platformconfig.RemoteAgentSettings, error)

// Disposition is the outcome of a successful Mint.
type Disposition int

const (
	// Issued: Token holds the assertion.
	Issued Disposition = iota + 1
	// Disabled: the run is not governed, and no assertion goes to this
	// audience (outbound assertions off, the audience not on the allowlist,
	// or no agent assertion key). The worker calls without one.
	Disabled
)

// MintResult is one answer.
type MintResult struct {
	Disposition Disposition
	Token       string
	ExpiresAt   time.Time
	// Required is true for a governed run: a call without an assertion is
	// refused by the worker.
	Required bool
}

// MintService decides and signs. Safe for concurrent use.
type MintService struct {
	runs     RunReader
	nested   NestedTools
	signer   Signer
	settings Settings
	audit    Auditor
}

// NewMintService builds the service. signer may be nil (no key ring): a
// run that is not governed is then answered Disabled at the cost of one run
// read (no settings read, no toolkit walk, no audit row), and a governed
// run is refused with ErrSignerUnavailable. nested may be nil: only the
// run's own toolkits are admitted audiences. settings should be cached by
// the caller (platformconfig.CachedSection): it is read on each mint of a
// run that is not governed.
func NewMintService(runs RunReader, nested NestedTools, signer Signer, settings Settings, audit Auditor) (*MintService, error) {
	if runs == nil || settings == nil || audit == nil {
		return nil, errors.New("remote agent: the mint service needs a run reader, settings and an auditor")
	}
	return &MintService{runs: runs, nested: nested, signer: signer, settings: settings, audit: audit}, nil
}

// Mint answers one request of a live claim (the caller verified the fence).
// A refusal carries Required when the run could be read.
func (s *MintService) Mint(ctx context.Context, executionID string, generation uint64, audience string) (MintResult, error) {
	run, err := s.runs.ReadRun(ctx, executionID, generation)
	if err != nil {
		return MintResult{}, fmt.Errorf("%w: %v", ErrRunUnreadable, err)
	}
	result := MintResult{Required: run.Governed}
	refuse := func(cause error, reason string) (MintResult, error) {
		if auditErr := s.audit.RecordRemoteAgent(ctx, s.event(run, ActionMintRefused, securityaudit.EffectDeny, reason, audience, "")); auditErr != nil {
			return result, fmt.Errorf("%w (and its audit failed: %v)", cause, auditErr)
		}
		return result, cause
	}
	// The worker sends the endpoint as configured; this is the only place
	// it is normalized, with the same function as the admitted URLs.
	normalized, ok := eliteanative.NormalizeAudience(audience)
	if !run.Governed {
		if s.signer == nil {
			return MintResult{Disposition: Disabled}, nil
		}
		settings, err := s.settings(ctx)
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrSettingsUnavailable, err)
		}
		if !outboundAllowed(settings, normalized, ok) {
			return MintResult{Disposition: Disabled}, nil
		}
	}
	if !ok {
		return refuse(ErrAudienceNotAdmitted, "audience_malformed")
	}
	admitted, err := s.admittedAudiences(ctx, run)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrRunUnreadable, err)
	}
	if !slices.Contains(admitted, normalized) {
		return refuse(ErrAudienceNotAdmitted, "audience_not_admitted")
	}
	if s.signer == nil {
		return refuse(ErrSignerUnavailable, "signer_unavailable")
	}
	scope, err := RunScope(run.Tools, run.ToolkitTypes)
	if err != nil {
		return refuse(ErrScopeTooLarge, "scope_too_large")
	}
	request := eliteanative.DelegationRequest{
		Audience: normalized, Agent: agentRef(run), TenantProjectID: run.TenantProjectID,
		ExecutionID: run.ExecutionID, Generation: run.Generation, MandateID: run.MandateID, Scope: scope,
		TTL: eliteanative.DelegationMaxTTL,
	}
	switch {
	case run.Governed:
		request.UserID = strings.TrimPrefix(run.SubjectPrincipalRef, "user:")
	default:
		request.UserID = run.ActorUserID
	}
	token, claims, err := s.signer.Sign(ctx, request)
	if errors.Is(err, eliteanative.ErrDelegationAudienceReserved) {
		return refuse(ErrAudienceNotAdmitted, "audience_reserved")
	}
	if err != nil {
		return refuse(fmt.Errorf("remote agent: sign: %w", err), "sign_failed")
	}
	if err := s.audit.RecordRemoteAgent(ctx, s.event(run, ActionMinted, securityaudit.EffectAllow, "minted", audience, claims.ID)); err != nil {
		// The security audit path never drops an event: an assertion whose
		// minting is not recorded is not handed out.
		return result, fmt.Errorf("remote agent: audit the mint: %w", err)
	}
	return MintResult{Disposition: Issued, Token: token, ExpiresAt: time.Unix(claims.Expiry, 0).UTC(), Required: run.Governed}, nil
}

// outboundAllowed applies outbound_assertions to the audience of a run
// that is not governed: "on" sends to every admitted endpoint, "allowlist"
// to the listed origins, "off" (and any other value) to none.
func outboundAllowed(settings platformconfig.RemoteAgentSettings, normalized string, ok bool) bool {
	switch settings.OutboundAssertions {
	case platformconfig.OutboundAssertionsOn:
		return true
	case platformconfig.OutboundAssertionsAllowlist:
		if !ok {
			return false
		}
		origin := eliteanative.AudienceOrigin(normalized)
		for _, entry := range settings.AssertionAudiences {
			if listed, valid := AudienceOrigin(entry); valid && listed == origin {
				return true
			}
		}
	}
	return false
}

// AudienceOrigin normalizes an entry of assertion_audiences: an https
// origin with no path. ok is false for anything else.
func AudienceOrigin(entry string) (string, bool) {
	normalized, ok := eliteanative.NormalizeAudience(strings.TrimSpace(entry))
	if !ok || eliteanative.AudienceOrigin(normalized) != normalized {
		return "", false
	}
	return normalized, true
}

func (s *MintService) event(run Run, action string, effect securityaudit.Effect, reason, audience, jti string) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeRemoteAgent, TenantProjectID: max(run.TenantProjectID, 0),
		Action: action, Effect: effect, Reasons: []string{reason},
		PrincipalRef: "agent:" + agentRef(run), AgentBindingID: run.AgentBindingID, MandateRef: run.MandateID,
		ExecutionID: run.ExecutionID, DecisionID: jti,
		ResourceType: "remote_endpoint", ResourceID: endpointHost(audience),
	}
}

// admittedAudiences lists the normalized endpoints of the run's MCP
// toolkits: its own frozen list and, with a nested reader, the frozen lists
// of the sub-agents it names (bounded like the admission's walk).
func (s *MintService) admittedAudiences(ctx context.Context, run Run) ([]string, error) {
	audiences, children := mcpAudiences(run.Tools)
	if s.nested == nil || len(children) == 0 {
		return audiences, nil
	}
	seen := map[[2]int64]bool{}
	frontier := children
	for depth := 0; len(frontier) > 0 && depth < maxNestedDepth; depth++ {
		var next [][2]int64
		for _, child := range frontier {
			if seen[child] {
				continue
			}
			seen[child] = true
			if len(seen) > maxNestedVersions {
				return audiences, nil
			}
			tools, err := s.nested.NestedApplicationTools(ctx, run.TenantProjectID, child[0], child[1])
			if err != nil {
				return nil, err
			}
			more, grandchildren := mcpAudiences(tools)
			audiences = append(audiences, more...)
			next = append(next, grandchildren...)
		}
		frontier = next
	}
	return audiences, nil
}

const (
	maxNestedDepth    = 25
	maxNestedVersions = 100
)

type frozenEntry struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	ToolkitName string `json:"toolkit_name"`
	Settings    struct {
		URL           string          `json:"url"`
		ApplicationID json.RawMessage `json:"application_id"`
		VersionID     json.RawMessage `json:"application_version_id"`
		SelectedTools []string        `json:"selected_tools"`
	} `json:"settings"`
}

func frozenEntries(raw json.RawMessage) []frozenEntry {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]frozenEntry, 0, len(items))
	for _, item := range items {
		var entry frozenEntry
		if json.Unmarshal(item, &entry) == nil {
			out = append(out, entry)
		}
	}
	return out
}

// IsMCPType is the worker's rule for a remote MCP toolkit type.
func IsMCPType(toolType string) bool {
	return toolType == "mcp" || toolType == "mcp_config" || strings.HasPrefix(toolType, "mcp_")
}

func mcpAudiences(raw json.RawMessage) ([]string, [][2]int64) {
	var audiences []string
	var children [][2]int64
	for _, entry := range frozenEntries(raw) {
		switch {
		case IsMCPType(entry.Type):
			if audience, ok := eliteanative.NormalizeAudience(entry.Settings.URL); ok {
				audiences = append(audiences, audience)
			}
		case entry.Type == "application":
			application, okA := positiveInt(entry.Settings.ApplicationID)
			version, okV := positiveInt(entry.Settings.VersionID)
			if okA && okV {
				children = append(children, [2]int64{application, version})
			}
		}
	}
	return audiences, children
}

func positiveInt(raw json.RawMessage) (int64, bool) {
	id, err := strconv.ParseInt(string(raw), 10, 64)
	return id, err == nil && id > 0
}

// RunScope is the elitea_scope claim of a run: its admitted toolkit types
// (sorted, unique) and a digest over its frozen toolkit list (each entry's
// type, name and selected tools, sorted).
func RunScope(tools json.RawMessage, toolkitTypes []string) (eliteanative.DelegationScope, error) {
	types := make([]string, 0, len(toolkitTypes))
	for _, t := range toolkitTypes {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	slices.Sort(types)
	types = slices.Compact(types)
	if len(types) > eliteanative.MaxScopeToolkits {
		return eliteanative.DelegationScope{}, ErrScopeTooLarge
	}
	lines := make([]string, 0)
	for _, entry := range frozenEntries(tools) {
		name := entry.ToolkitName
		if name == "" {
			name = entry.Name
		}
		selected := slices.Clone(entry.Settings.SelectedTools)
		slices.Sort(selected)
		lines = append(lines, entry.Type+"\x00"+name+"\x00"+strings.Join(selected, "\x00"))
	}
	slices.Sort(lines)
	digest := sha256.New()
	digest.Write([]byte("elitea.remote-agent.scope.v1\n"))
	for _, t := range types {
		digest.Write([]byte("type\x00" + t + "\n"))
	}
	for _, line := range lines {
		digest.Write([]byte("toolkit\x00" + line + "\n"))
	}
	scope := eliteanative.DelegationScope{Digest: "sha256:" + hex.EncodeToString(digest.Sum(nil)), Toolkits: types}
	if !eliteanative.ValidScope(scope) {
		return eliteanative.DelegationScope{}, ErrScopeTooLarge
	}
	return scope, nil
}

// agentRef names the calling agent: its identity binding when governed,
// else the application, else an ad hoc chat of the project.
func agentRef(run Run) string {
	project := strconv.FormatInt(run.TenantProjectID, 10)
	switch {
	case run.AgentBindingID != "":
		return "binding:" + run.AgentBindingID
	case run.ApplicationID > 0:
		return "application:" + project + ":" + strconv.FormatInt(run.ApplicationID, 10)
	default:
		return "adhoc:" + project
	}
}

// endpointHost is what an audit event names of an endpoint: its host, never
// its path (which may name a tenant).
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || len(u.Host) > 253 {
		return ""
	}
	return strings.ToLower(u.Host)
}
