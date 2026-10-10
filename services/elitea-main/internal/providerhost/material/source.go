package material

// Expanding a SOURCE TOOLKIT an invocation names (ADR-0022 §6).
//
// The pattern DeepWiki established against a configuration row, generalised
// against a toolkit row because Inventory needs it: a client names an id, the
// facade decides whether that id may be expanded AT ALL, checks where the
// resulting clone would go, and only then opens the vault — projecting a
// named set of fields into the forwarded body.
//
// THE ORDER IS THE CONTRACT and it is the whole point of this file:
//
//	admitted? → a known and allowed type? → host on the allowlist? → decrypt.
//
// Each step refuses without doing the next one's work. A source the invoking
// toolkit does not name never reaches a type check; a type nobody allowed
// never reaches an egress check; a host nobody allowed never reaches a vault.
//
// WHAT IS PARAMETERISED, and what is not. The Kinds table, the field names and
// the refusal text belong to a provider — they are what its engine reads. The
// four-step order does not, and no caller can reorder it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

var (
	// ErrSourceNotAdmitted reports an id the invoking toolkit's own list does
	// not name. 403: the caller may invoke, but not against that source.
	ErrSourceNotAdmitted = errors.New("source is not one of this toolkit's sources")
	// ErrSourceRefused reports a source this facade will not expand — a type
	// off the allowlist, a row that is not a toolkit, a malformed id.
	ErrSourceRefused = errors.New("source cannot be expanded")
	// ErrSourceUnavailable reports a failure the caller cannot fix.
	ErrSourceUnavailable = errors.New("source credentials are unavailable")
)

// Kind is one expandable source type: the credential block its SDK loader
// reads, the field inside it carrying the host, and the plain settings that
// cross with it.
type Kind struct {
	Configuration string
	HostField     string
	DefaultHost   string
	// Credentials and Settings are ENUMERATIONS, not filters. A source
	// toolkit's settings also hold a PgVector connection string and an
	// embedding model, and a provider has no business receiving either.
	Credentials []string
	Settings    []string
}

// ToolkitReader reads one toolkit row from an authorized project. Satisfied by
// repos.CurrentToolkitsRepository; narrowed to the one method used.
type ToolkitReader interface {
	Get(ctx context.Context, projectID, toolkitID int32) (repos.CurrentToolkit, error)
}

// SettingsResolver expands a toolkit's saved settings. Satisfied by
// configurations.CurrentToolkitSettingsResolver — the SAME resolver the index
// scheduler claims credentials with, so what a source toolkit's own runs read
// is what a provider receives.
type SettingsResolver interface {
	Resolve(ctx context.Context, request configurationapp.CurrentToolkitSettingsRequest) (map[string]any, error)
}

// Expander turns one source toolkit id into the fields a provider may have.
type Expander struct {
	Toolkits ToolkitReader
	Settings SettingsResolver
	Egress   GitEgressPolicy
	// Kinds is what this provider knows how to project, by toolkit type.
	Kinds map[string]Kind
	// Allowed is what this DEPLOYMENT permits of those. A type with no Kind
	// cannot be allowed by configuration: allowing it would mean projecting
	// fields nobody enumerated.
	Allowed []string
	// SourcesField is the key in the INVOKING toolkit's settings holding the
	// ids it may expand.
	SourcesField string
	// AllowlistEnv names the variable Allowed came from, for a refusal.
	AllowlistEnv string
}

// Expand runs the four steps, in order.
//
// It returns the projected source AND the invoking toolkit's own settings,
// because a caller's per-source configuration lives there and re-reading the
// row to find it would be a second query for a value already in hand.
func (e Expander) Expand(
	ctx context.Context,
	project, user, ownerID, sourceID int32,
) (source map[string]any, ownerSettings map[string]any, err error) {
	if e.Toolkits == nil || e.Settings == nil {
		return nil, nil, ErrSourceUnavailable
	}
	ownerRow, err := e.Toolkits.Get(ctx, project, ownerID)
	if err != nil {
		return nil, nil, toolkitError(err, ownerID)
	}
	ownerSettings = ObjectOf(ownerRow.Settings)
	if !admits(ownerSettings[e.SourcesField], sourceID) {
		return nil, nil, fmt.Errorf("%w: toolkit %d is not in toolkit %d's %s",
			ErrSourceNotAdmitted, sourceID, ownerID, e.SourcesField)
	}

	sourceRow, err := e.Toolkits.Get(ctx, project, sourceID)
	if err != nil {
		return nil, nil, toolkitError(err, sourceID)
	}
	kind, known := e.Kinds[strings.ToLower(sourceRow.Type)]
	if !known || !allowed(e.Allowed, sourceRow.Type) {
		return nil, nil, fmt.Errorf(
			"%w: toolkit %d is a %q toolkit, which %s does not allow as a source",
			ErrSourceRefused, sourceID, sourceRow.Type, e.AllowlistEnv)
	}

	// REFERENCE mode first, and it is not a duplicate read: it expands the
	// source's configuration reference with its secrets still SEALED — no
	// vault is opened — and it exists so the host can be read before the
	// claim-mode call decrypts. A refused host returns having decrypted
	// nothing, which is only true because of the order of these two calls.
	request := configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: sourceRow.Type,
		ProjectID:   project,
		UserID:      user,
		Settings:    ObjectOf(sourceRow.Settings),
		Mode:        configurationapp.CurrentToolkitSettingsReferenceMode,
	}
	sealed, err := e.Settings.Resolve(ctx, request)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrSourceUnavailable, err)
	}
	if err := e.Egress.Allow(hostOf(sealed, kind)); err != nil {
		return nil, nil, err
	}

	request.Settings = ObjectOf(sourceRow.Settings)
	request.Mode = configurationapp.CurrentToolkitSettingsClaimMode
	claimed, err := e.Settings.Resolve(ctx, request)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrSourceUnavailable, err)
	}

	source = map[string]any{"toolkit_id": sourceID, "type": sourceRow.Type}
	if sourceRow.Name != nil {
		source["name"] = *sourceRow.Name
	}
	source[kind.Configuration] = pick(ObjectOf(claimed[kind.Configuration]), kind.Credentials)
	for key, value := range pick(claimed, kind.Settings) {
		source[key] = value
	}
	return source, ownerSettings, nil
}

// SourceRewriter is the invoke-body rewrite built on one Expander: the client
// names a source id, the provider receives the source.
type SourceRewriter struct {
	Provider     string
	Expander     Expander
	Minter       Minter
	CallbackBase string
	Lifetime     time.Duration

	// OwnerField is the key in the envelope's `configuration` naming the
	// INVOKING toolkit's own row (Inventory: `application_id`).
	OwnerField string
	// SourceArg is the TOOL argument naming the source (Inventory:
	// `toolkit_id`).
	SourceArg string
	// OutputField is where the expanded source lands in
	// `configuration.parameters` (Inventory: `source`).
	OutputField string
	// Decorate, when set, adjusts the projected source from the invoking
	// toolkit's own settings and the tool's arguments.
	Decorate func(source, ownerSettings map[string]any, tool map[string]json.RawMessage, sourceID int32)
	// Grants records what a grant-only tool's bearer was minted for
	// (GrantRewriteFor); nil refuses the recorded tool rather than minting
	// a bearer the source-tool gate cannot recognise.
	Grants GrantRecorder
}

// Rewrite reads the body and returns the one to forward, plus the grant it
// minted so a failed hop can revoke it.
func (rw SourceRewriter) Rewrite(
	ctx context.Context,
	body io.Reader,
	projectID, userID int64,
) ([]byte, Grant, error) {
	envelope, err := Read(body)
	if err != nil {
		return nil, Grant{}, err
	}
	project, projectOK := NarrowRowID(projectID)
	user, userOK := NarrowRowID(userID)
	if !projectOK || !userOK {
		return nil, Grant{}, fmt.Errorf("%w: project %d user %d is out of range",
			ErrSourceRefused, projectID, userID)
	}
	// The client chooses WHICH of its own project's toolkits it is invoking;
	// what that toolkit may reach is read from the row, never from the body.
	ownerID, err := RowID(envelope.Configuration()[rw.OwnerField], "configuration."+rw.OwnerField)
	if err != nil {
		return nil, Grant{}, err
	}
	tool, err := envelope.ToolParameters()
	if err != nil {
		return nil, Grant{}, err
	}
	sourceID, err := RowID(
		firstPresent(tool[rw.SourceArg], envelope.Parameters()[rw.SourceArg]),
		"parameters."+rw.SourceArg)
	if err != nil {
		return nil, Grant{}, err
	}
	source, ownerSettings, err := rw.Expander.Expand(ctx, project, user, ownerID, sourceID)
	if err != nil {
		return nil, Grant{}, err
	}
	if rw.Decorate != nil {
		rw.Decorate(source, ownerSettings, tool, sourceID)
	}

	// The token LAST. Resolution is the step that refuses, and minting before
	// it would leave a live bearer behind for every refused request — a
	// credential issued for work that never happened.
	grant, err := rw.Minter.Mint(ctx, userID, projectID,
		fmt.Sprintf("%s callback (project %d)", rw.Provider, projectID), rw.Lifetime)
	if err != nil {
		return nil, Grant{}, err
	}
	if err := envelope.Set(rw.OutputField, source); err != nil {
		return nil, grant, err
	}
	block := CallbackSettings(rw.CallbackBase, grant, projectID,
		String(envelope.Parameters(), "llm_model"))
	if err := envelope.LiftToolLLMSettings(block); err != nil {
		return nil, grant, err
	}
	if err := envelope.Set("llm_settings", block); err != nil {
		return nil, grant, err
	}
	rewritten, err := envelope.Encode()
	if err != nil {
		return nil, grant, err
	}
	return rewritten, grant, nil
}

// admits reports whether the invoking toolkit's own list names this id. THE
// GATE, and the only reason a caller cannot name any toolkit in the project.
func admits(sources any, sourceID int32) bool {
	list, ok := sources.([]any)
	if !ok {
		return false
	}
	for _, entry := range list {
		if id, ok := RowIDOf(entry); ok && id == sourceID {
			return true
		}
	}
	return false
}

func allowed(list []string, toolkitType string) bool {
	for _, entry := range list {
		if strings.EqualFold(entry, toolkitType) {
			return true
		}
	}
	return false
}

// hostOf reads the host a clone would reach, from settings whose secrets are
// still sealed. Empty — which every allowlist refuses — for a host built from
// a secret: such a host cannot be checked before the decrypt that would
// reveal it, so it has no safe order at all.
func hostOf(settings map[string]any, kind Kind) string {
	raw := Text(ObjectOf(settings[kind.Configuration])[kind.HostField])
	if raw == "" {
		raw = kind.DefaultHost
	}
	if raw == "" || strings.Contains(raw, "{{secret.") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func pick(from map[string]any, keys []string) map[string]any {
	picked := map[string]any{}
	for _, key := range keys {
		if value, ok := from[key]; ok && value != nil {
			picked[key] = value
		}
	}
	return picked
}

// ObjectOf reads a JSON object out of a decoded `any`, or an empty one.
func ObjectOf(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// Text reads a trimmed string out of a decoded `any`, or "".
func Text(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// RowIDOf reads an id out of decoded JSON, which carries integers as
// json.Number (the toolkit repository keeps them out of float64 on purpose).
func RowIDOf(value any) (int32, bool) {
	var parsed int64
	var err error
	switch typed := value.(type) {
	case json.Number:
		parsed, err = typed.Int64()
	case float64:
		parsed = int64(typed)
	case string:
		parsed, err = strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	return NarrowRowID(parsed)
}

func firstPresent(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && !IsNull(value) {
			return value
		}
	}
	return nil
}

func toolkitError(err error, toolkitID int32) error {
	if errors.Is(err, repos.ErrCurrentToolkitNotFound) ||
		errors.Is(err, repos.ErrInvalidCurrentToolkitRequest) {
		return fmt.Errorf("%w: toolkit %d", ErrSourceRefused, toolkitID)
	}
	return fmt.Errorf("%w: %s", ErrSourceUnavailable, err)
}
