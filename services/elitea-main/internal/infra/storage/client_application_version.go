package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

const (
	// ClientApplicationVersionSchemaVersion names the desktop's resolved
	// definition (ADR-0029 decision 5a, client contract 1.6). It is a separate
	// discriminator from RuntimeApplicationVersionSchemaVersion on purpose: the
	// two documents start from the same frozen projection, but this one has had
	// every credential removed, so a worker must never accept it and a client
	// must never be handed the worker's.
	ClientApplicationVersionSchemaVersion = "elitea.client.resolved-application-version.v1"

	// ClientToolkitRefPrefix starts every opaque toolkit reference.
	ClientToolkitRefPrefix = "tkr1_"

	// clientApplicationVersionDigestDomain separates this digest from every
	// other SHA-256 in the platform, the worker's definition digest included.
	clientApplicationVersionDigestDomain = "elitea.client.resolved-application-version.v1\x00"
	clientToolkitRefDomain               = "elitea.client.toolkit-ref.v2\x00"

	// The tool kinds a resolved definition carries.
	ClientToolKindRemoteToolkit = "remote_toolkit"
	ClientToolKindApplication   = "application"
	ClientToolKindPlatformMCP   = "platform_mcp"

	// clientSecretWithheld replaces every `{{secret.*}}` reference left outside
	// the tools (an authored variable value, an instruction): the desktop has no
	// way to redeem one, and the reference names a vault entry.
	clientSecretWithheld = "[secret withheld]"
)

var (
	// ErrClientApplicationVersionUnresolvable is a saved version the shared
	// admission freeze refuses: the version a cloud turn could not start either
	// (an unsealed secret, a credential the caller cannot see, no model).
	ErrClientApplicationVersionUnresolvable = errors.New("application version cannot be resolved for execution")

	clientSecretReference = regexp.MustCompile(`\{\{\s*secret\.[^{}]*\}\}`)
)

// ClientApplicationVersion is the wire document of resolveApplicationVersion.
type ClientApplicationVersion struct {
	SchemaVersion    string          `json:"schema_version"`
	ProjectID        int64           `json:"project_id"`
	ApplicationID    int64           `json:"application_id"`
	VersionID        int64           `json:"version_id"`
	VersionDetails   json.RawMessage `json:"version_details"`
	DefinitionSHA256 string          `json:"definition_sha256"`
	// WithheldSecrets are the JSON Pointers (RFC 6901, into version_details)
	// of every string whose `{{secret.*}}` reference was replaced with
	// "[secret withheld]". Never null. A desktop that finds one here knows the
	// local run would see the placeholder, not the value, and can run that
	// agent in the cloud instead.
	WithheldSecrets []string `json:"withheld_secrets"`
	// ProjectContextWithheld is true when the version's frozen project
	// context was removed because the caller may not read it
	// (`models.project_context.view`). A local run then lacks the context a
	// cloud turn would have.
	ProjectContextWithheld bool `json:"project_context_withheld"`
}

// WithoutProjectContext answers the document with version_details'
// `project_context` removed (and the digest recomputed over what remains),
// flagged. A document that carries no project context is returned unchanged.
func (document ClientApplicationVersion) WithoutProjectContext() (ClientApplicationVersion, error) {
	decoder := json.NewDecoder(bytes.NewReader(document.VersionDetails))
	decoder.UseNumber()
	var details map[string]any
	if err := decoder.Decode(&details); err != nil || details == nil {
		return ClientApplicationVersion{}, errors.New("resolved version details are not one JSON object")
	}
	if _, present := details["project_context"]; !present {
		return document, nil
	}
	delete(details, "project_context")
	encoded, err := json.Marshal(details)
	if err != nil {
		return ClientApplicationVersion{}, err
	}
	withheld := make([]string, 0, len(document.WithheldSecrets))
	for _, pointer := range document.WithheldSecrets {
		if pointer != "/project_context" && !strings.HasPrefix(pointer, "/project_context/") {
			withheld = append(withheld, pointer)
		}
	}
	document.VersionDetails = encoded
	document.WithheldSecrets = withheld
	document.ProjectContextWithheld = true
	document.DefinitionSHA256 = clientApplicationVersionSHA256(
		document.ProjectID, uint64(document.ApplicationID), uint64(document.VersionID), encoded)
	return document, nil
}

// ClientApplicationVersionService serves one agent or pipeline version to a
// desktop client fully resolved for execution, with no secret material
// (ADR-0029 decision 5a).
//
// It shares freezeSavedApplicationVersion with RuntimeApplicationVersionService
// — the same read, the same identity check, the same freezer the interactive
// start path uses — and differs only in what follows the freeze. The worker's
// route REDEEMS the sealed references for a claim. This one never does: it
// replaces each toolkit's settings with an opaque reference the desktop sends
// back to executeRemoteToolkitTool, which re-resolves the saved toolkit
// server-side. There is no materializer dependency, so there is no code path
// here that could produce plaintext.
type ClientApplicationVersionService struct {
	versions CurrentApplicationVersionSource
	freezer  agentexecutionapp.CurrentApplicationVersionFreezer
	// guardrails serves AuthorizeRemoteTool only (WithGuardrails).
	guardrails ClientGuardrailResolver
}

func NewClientApplicationVersionService(
	versions CurrentApplicationVersionSource,
	freezer agentexecutionapp.CurrentApplicationVersionFreezer,
) (*ClientApplicationVersionService, error) {
	if versions == nil || freezer == nil {
		return nil, errors.New("client application version dependencies are required")
	}
	return &ClientApplicationVersionService{versions: versions, freezer: freezer}, nil
}

// Resolve answers the resolved definition of one version in projectID for
// actorID. The caller has already authenticated the actor and checked the
// version-read permission in that project; the tenant schema the project
// selects is what scopes the read.
//
// Errors: ErrContentNotFound for an absent pair,
// ErrClientApplicationVersionUnresolvable for a version the freeze refuses,
// ErrContentUnavailable for a dependency fault, or the context's error.
func (service *ClientApplicationVersionService) Resolve(
	ctx context.Context,
	projectID int64,
	actorID int64,
	applicationID uint64,
	versionID uint64,
) (ClientApplicationVersion, error) {
	if service == nil || service.versions == nil || service.freezer == nil || ctx == nil {
		return ClientApplicationVersion{}, ErrContentUnavailable
	}
	if err := ctx.Err(); err != nil {
		return ClientApplicationVersion{}, err
	}
	if projectID <= 0 || projectID > math.MaxInt32 || actorID <= 0 || actorID > math.MaxInt32 {
		return ClientApplicationVersion{}, ErrContentNotFound
	}
	record, frozen, err := freezeSavedApplicationVersion(
		ctx, service.versions, service.freezer, projectID, actorID, applicationID, versionID,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ClientApplicationVersion{}, contextErr
		}
		if errors.Is(err, ErrContentNotFound) {
			return ClientApplicationVersion{}, ErrContentNotFound
		}
		var stage *savedApplicationVersionFreezeError
		if errors.As(err, &stage) && !stage.read &&
			errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) &&
			!errors.Is(err, configurationapp.ErrCurrentToolkitSettingsDependency) {
			return ClientApplicationVersion{}, ErrClientApplicationVersionUnresolvable
		}
		return ClientApplicationVersion{}, ErrContentUnavailable
	}
	identity := ClientVersionIdentity{ProjectID: projectID, ApplicationID: int64(applicationID), VersionID: int64(versionID)}
	projected, withheld, err := ProjectClientApplicationVersion(identity, frozen, record.VersionDetails)
	clearContentBytes(frozen)
	if err != nil {
		return ClientApplicationVersion{}, ErrClientApplicationVersionUnresolvable
	}
	return ClientApplicationVersion{
		SchemaVersion:    ClientApplicationVersionSchemaVersion,
		ProjectID:        projectID,
		ApplicationID:    int64(applicationID),
		VersionID:        int64(versionID),
		VersionDetails:   projected,
		DefinitionSHA256: clientApplicationVersionSHA256(projectID, applicationID, versionID, projected),
		WithheldSecrets:  withheld,
	}, nil
}

// ProjectClientApplicationVersion turns one FROZEN version (the freezer's
// output, secret references still sealed) into the desktop's document.
//
// Every tool is rebuilt from an allowlist, never copied and then pruned: a
// field this function does not name cannot reach the client, so a credential
// a future toolkit schema adds to `settings` or `meta` is withheld by
// construction. Outside the tools, every `{{secret.*}}` reference is replaced
// and the frozen-configuration marker is dropped wherever it appears.
//
// saved is the SAVED version the freeze started from (nil means the frozen
// one): a remote toolkit's selection is the frozen one, and saved tells a
// selection the author left empty ("every tool", all_tools true) from one the
// freeze emptied by removing every selected tool as blocked (no tool,
// all_tools false). clientToolkitSelection is the rule, shared with
// AuthorizeRemoteTool.
//
// It also answers the JSON Pointers of every replaced reference, sorted.
func ProjectClientApplicationVersion(identity ClientVersionIdentity, frozen, saved json.RawMessage) (json.RawMessage, []string, error) {
	if !identity.valid() || len(frozen) == 0 ||
		len(frozen) > maxRuntimeApplicationVersionResponseBytes*8 {
		return nil, nil, errors.New("frozen application version is invalid")
	}
	if len(saved) == 0 {
		saved = frozen
	}
	savedSelections, err := clientToolkitSelections(saved)
	if err != nil {
		return nil, nil, errors.New("saved application version is not one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(frozen))
	decoder.UseNumber()
	var version map[string]any
	if err := decoder.Decode(&version); err != nil || version == nil {
		return nil, nil, errors.New("frozen application version is not one JSON object")
	}
	tools, ok := version["tools"].([]any)
	if !ok {
		return nil, nil, errors.New("frozen application version has no tools array")
	}
	projectedTools := make([]any, 0, len(tools))
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			return nil, nil, errors.New("a frozen tool is not an object")
		}
		projectedTool, ok := projectClientTool(identity, tool, savedSelections)
		if !ok {
			return nil, nil, errors.New("a frozen tool could not be projected")
		}
		projectedTools = append(projectedTools, projectedTool)
	}
	version["tools"] = projectedTools
	withheld := []string{}
	scrubbed := scrubClientValue(version, "", &withheld)
	sort.Strings(withheld)
	encoded, err := json.Marshal(scrubbed)
	if err != nil || len(encoded) == 0 || len(encoded) > maxRuntimeApplicationVersionResponseBytes {
		return nil, nil, errors.New("resolved application version is unencodable or too large")
	}
	return encoded, withheld, nil
}

// ClientVersionIdentity names one agent version in one project.
type ClientVersionIdentity struct {
	ProjectID     int64
	ApplicationID int64
	VersionID     int64
}

func (identity ClientVersionIdentity) valid() bool {
	for _, id := range []int64{identity.ProjectID, identity.ApplicationID, identity.VersionID} {
		if id <= 0 || id > math.MaxInt32 {
			return false
		}
	}
	return true
}

// ClientToolkitRef is the stable opaque reference of one saved toolkit AS
// ATTACHED TO one agent version in one project. It is derived, not stored: the
// same attachment always has the same reference. It is not a secret and grants
// nothing; executeRemoteToolkitTool recomputes it from the version the call
// names and refuses a mismatch, so a reference copied from another version,
// project or toolkit type cannot be replayed against this one.
func ClientToolkitRef(identity ClientVersionIdentity, toolkitID int64, toolkitType string) string {
	var identities [32]byte
	binary.BigEndian.PutUint64(identities[0:8], uint64(identity.ProjectID))
	binary.BigEndian.PutUint64(identities[8:16], uint64(identity.ApplicationID))
	binary.BigEndian.PutUint64(identities[16:24], uint64(identity.VersionID))
	binary.BigEndian.PutUint64(identities[24:32], uint64(toolkitID))
	digest := sha256.New()
	_, _ = digest.Write([]byte(clientToolkitRefDomain))
	_, _ = digest.Write(identities[:])
	_, _ = digest.Write([]byte(toolkitType))
	return ClientToolkitRefPrefix + hex.EncodeToString(digest.Sum(nil)[:16])
}

func projectClientTool(identity ClientVersionIdentity, tool map[string]any, savedSelections map[int64][]string) (map[string]any, bool) {
	toolType, ok := tool["type"].(string)
	if !ok || toolType == "" {
		return nil, false
	}
	common := func(kind string) map[string]any {
		projected := map[string]any{"kind": kind, "type": toolType}
		for _, key := range []string{"name", "toolkit_name", "description"} {
			if text, ok := tool[key].(string); ok {
				projected[key] = text
			} else {
				projected[key] = nil
			}
		}
		return projected
	}
	if toolType == "application" {
		settings, ok := tool["settings"].(map[string]any)
		if !ok {
			return nil, false
		}
		applicationID, okApplication := positiveClientJSONInteger(settings["application_id"])
		versionID, okVersion := positiveClientJSONInteger(settings["application_version_id"])
		if !okApplication || !okVersion {
			return nil, false
		}
		projected := common(ClientToolKindApplication)
		if id, ok := positiveClientJSONInteger(tool["id"]); ok {
			projected["id"] = id
		} else {
			projected["id"] = nil
		}
		if agentType, ok := tool["agent_type"].(string); ok {
			projected["agent_type"] = agentType
		}
		projected["application_id"] = applicationID
		projected["application_version_id"] = versionID
		if registry, ok := tool["nested_skill_registry"]; ok && registry != nil {
			projected["nested_skill_registry"] = registry
		}
		return projected, true
	}
	if _, internal := mcpregistry.InternalBuilderForType(toolType); internal {
		projected := common(ClientToolKindPlatformMCP)
		projected["server_name"] = toolType
		return projected, true
	}
	toolkitID, ok := positiveClientJSONInteger(tool["id"])
	if !ok {
		return nil, false
	}
	projected := common(ClientToolKindRemoteToolkit)
	projected["id"] = toolkitID
	frozenSelection := selectedToolNames(tool)
	savedSelection, known := savedSelections[toolkitID]
	if !known {
		savedSelection = frozenSelection
	}
	selected, allTools := clientToolkitSelection(savedSelection, frozenSelection)
	projected["selected_tools"] = selected
	projected["all_tools"] = allTools
	projected["toolkit_ref"] = map[string]any{
		"toolkit_id": toolkitID,
		"project_id": identity.ProjectID,
		"ref":        ClientToolkitRef(identity, toolkitID, toolType),
	}
	return projected, true
}

// scrubClientValue walks a decoded JSON value: it drops the frozen
// configuration marker and replaces every secret reference inside a string,
// recording the JSON Pointer of each string it changed.
func scrubClientValue(value any, path string, withheld *[]string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == configurationapp.CurrentFrozenConfigurationMarker {
				delete(typed, key)
				continue
			}
			typed[key] = scrubClientValue(item, path+"/"+jsonPointerToken(key), withheld)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = scrubClientValue(item, path+"/"+strconv.Itoa(index), withheld)
		}
		return typed
	case string:
		if !strings.Contains(typed, "{{") {
			return typed
		}
		replaced := clientSecretReference.ReplaceAllString(typed, clientSecretWithheld)
		if replaced != typed {
			*withheld = append(*withheld, path)
		}
		return replaced
	default:
		return value
	}
}

// jsonPointerToken escapes one RFC 6901 reference token.
func jsonPointerToken(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func positiveClientJSONInteger(value any) (int64, bool) {
	var parsed int64
	switch typed := value.(type) {
	case json.Number:
		number, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = number
	case float64:
		if typed != math.Trunc(typed) || typed > math.MaxInt32 {
			return 0, false
		}
		parsed = int64(typed)
	case int64:
		parsed = typed
	case int32:
		parsed = int64(typed)
	case int:
		parsed = int64(typed)
	default:
		return 0, false
	}
	if parsed <= 0 || parsed > math.MaxInt32 {
		return 0, false
	}
	return parsed, true
}

func clientApplicationVersionSHA256(projectID int64, applicationID, versionID uint64, document []byte) string {
	var identities [32]byte
	binary.BigEndian.PutUint64(identities[0:8], uint64(projectID))
	binary.BigEndian.PutUint64(identities[8:16], applicationID)
	binary.BigEndian.PutUint64(identities[16:24], versionID)
	binary.BigEndian.PutUint64(identities[24:32], uint64(len(document)))
	digest := sha256.New()
	_, _ = digest.Write([]byte(clientApplicationVersionDigestDomain))
	_, _ = digest.Write(identities[:])
	_, _ = digest.Write(document)
	return hex.EncodeToString(digest.Sum(nil))
}

// clientToolkitSelection is a remote toolkit's selection as the desktop reads
// it and as AuthorizeRemoteTool admits a call: the FROZEN selection, which the
// freeze has stripped of blocked names. allTools is true only when the author
// selected nothing at all, which the SDK reads as every tool. A selection the
// freeze EMPTIED (every selected tool blocked) is no tool: selected is empty
// and allTools false, never read as "every tool".
func clientToolkitSelection(saved, frozen []string) (selected []string, allTools bool) {
	if len(frozen) > 0 {
		return append([]string{}, frozen...), false
	}
	return []string{}, len(saved) == 0
}

// selectedToolNames reads one tool entry's settings.selected_tools.
func selectedToolNames(tool map[string]any) []string {
	var selected []string
	if settings, ok := tool["settings"].(map[string]any); ok {
		if values, ok := settings["selected_tools"].([]any); ok {
			for _, value := range values {
				if name, ok := value.(string); ok && name != "" {
					selected = append(selected, name)
				}
			}
		}
	}
	return selected
}

// clientToolkitSelections maps each toolkit id of a version document to its
// selected_tools.
func clientToolkitSelections(document json.RawMessage) (map[int64][]string, error) {
	tools, err := decodeClientTools(document)
	if err != nil {
		return nil, err
	}
	selections := make(map[int64][]string, len(tools))
	for _, tool := range tools {
		if id, ok := positiveClientJSONInteger(tool["id"]); ok {
			if _, seen := selections[id]; !seen {
				selections[id] = selectedToolNames(tool)
			}
		}
	}
	return selections, nil
}
