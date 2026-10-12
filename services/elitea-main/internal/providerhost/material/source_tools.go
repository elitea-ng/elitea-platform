package material

// A provider agent calling its SOURCE toolkits' read-only tools back through
// the platform (ADR-0027 P4, Inventory `investigate`).
//
// The engine calls `POST /api/v2/elitea_core/test_tool/prompt_lib/{p}/{t}`
// with the callback bearer minted for its invocation. That route takes
// `models.applications.tool.patch` — an EDIT of the toolkit — so a user who may
// chat but not edit toolkits got 403 on every source call. Owner decision:
// let the invocation's own grant through, and nothing else.
//
// TWO HALVES, and both are needed:
//
//   - the MINT records what the token is for (SourceRewriter.GrantRewriteFor):
//     the provider, the tool, the invoking toolkit and the source toolkits its
//     `sources` list names, in elitea_identity.callback_token_grant. A token's
//     name is no evidence — any user can make a PAT with any name — so the
//     record is the only thing that separates a callback bearer from a PAT;
//
//   - the GATE (SourceToolGate) admits a test_tool call with the chat-time
//     execute permission instead of patch only when the AUTHENTICATING token
//     carries such a record for this project, this caller, this toolkit, is
//     unexpired, and the tool is read-only by the engine's own rule. Every
//     other request — a session, a PAT, a callback token for another tool,
//     project or toolkit, a write tool, a body the gate cannot read — takes
//     the route's ordinary gate unchanged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// GrantRecorder writes what a callback token was minted for
// (*repos.CallbackTokenGrants).
type GrantRecorder interface {
	Record(ctx context.Context, grant repos.CallbackTokenGrant) error
}

// SourceToolGrants reads it back (*repos.CallbackTokenGrants).
type SourceToolGrants interface {
	AdmitsSourceTool(ctx context.Context, projectID, userID, tokenID, toolkitID int64, provider, tool string) (bool, error)
}

var (
	_ GrantRecorder    = (*repos.CallbackTokenGrants)(nil)
	_ SourceToolGrants = (*repos.CallbackTokenGrants)(nil)
)

// GrantRewriteFor is the RewriteFor of the grant-only tools: each gets the
// callback block (CallbackOnly), and sourceTool's grant is also RECORDED with
// the invoking toolkit's source toolkits, so its bearer may call their
// read-only tools (SourceToolGate). sourceTool must be one of tools.
func (rw SourceRewriter) GrantRewriteFor(sourceTool string, tools ...string) func(toolkitName, toolName string) Rewriter {
	plain := CallbackOnly(rw.Provider, rw.Minter, rw.CallbackBase, rw.Lifetime)
	recorded := rw.recordedGrant(sourceTool)
	return func(_, toolName string) Rewriter {
		switch {
		case toolName == sourceTool && slices.Contains(tools, toolName):
			return recorded
		case slices.Contains(tools, toolName):
			return plain
		}
		return nil
	}
}

func (rw SourceRewriter) recordedGrant(tool string) Rewriter {
	return func(ctx context.Context, body io.Reader, projectID, userID int64) ([]byte, Grant, error) {
		envelope, err := Read(body)
		if err != nil {
			return nil, Grant{}, err
		}
		project, projectOK := NarrowRowID(projectID)
		if _, userOK := NarrowRowID(userID); !projectOK || !userOK {
			return nil, Grant{}, fmt.Errorf("%w: project %d user %d is out of range",
				ErrSourceRefused, projectID, userID)
		}
		owner, sources, err := rw.grantSources(ctx, envelope, project)
		if err != nil {
			return nil, Grant{}, err
		}
		// The token after the reads, as everywhere: a refused read leaves no
		// bearer behind.
		rewritten, grant, err := Settle(ctx, envelope, rw.Minter,
			rw.Provider, rw.CallbackBase, rw.Lifetime, projectID, userID)
		if err != nil || owner == 0 {
			// No invoking toolkit named: the bearer reaches the model and
			// nothing else; source calls take the route's ordinary gate.
			return rewritten, grant, err
		}
		if rw.Grants == nil {
			return nil, grant, fmt.Errorf("%w: no grant recorder", ErrSourceUnavailable)
		}
		if err := rw.Grants.Record(ctx, repos.CallbackTokenGrant{
			TokenUUID: grant.UUID, OwnerID: userID, ProjectID: projectID,
			Provider: rw.Provider, Tool: tool,
			OwnerToolkitID: owner, SourceToolkitIDs: sources,
		}); err != nil {
			// Returned WITH the grant, so the handler revokes the bearer.
			return nil, grant, fmt.Errorf("%w: %s", ErrSourceUnavailable, err)
		}
		return rewritten, grant, nil
	}
}

// grantSources reads the invoking toolkit (OwnerField, where the host reads
// it: configuration first, then the tool's and the toolkit's parameters) and
// the source toolkits its own list names whose type this facade could ever
// have ingested. 0 means the body names no invoking toolkit.
func (rw SourceRewriter) grantSources(ctx context.Context, envelope *Envelope, project int32) (int32, []int32, error) {
	tool, err := envelope.ToolParameters()
	if err != nil {
		return 0, nil, err
	}
	encoded := firstPresent(envelope.Configuration()[rw.OwnerField], tool[rw.OwnerField],
		envelope.Parameters()[rw.OwnerField])
	if encoded == nil {
		return 0, nil, nil
	}
	owner, err := RowID(encoded, rw.OwnerField)
	if err != nil {
		return 0, nil, err
	}
	toolkits := rw.Expander.Toolkits
	if toolkits == nil {
		return 0, nil, ErrSourceUnavailable
	}
	row, err := toolkits.Get(ctx, project, owner)
	if err != nil {
		return 0, nil, toolkitError(err, owner)
	}
	list, _ := ObjectOf(row.Settings)[rw.Expander.SourcesField].([]any)
	sources := []int32{}
	lookups := 0
	for _, entry := range list {
		id, ok := RowIDOf(entry)
		if !ok || id == owner || slices.Contains(sources, id) {
			continue
		}
		// One read per listed source (the reader has no by-ids read), so the
		// list is bounded: a sources list past the cap is cut, not looked up
		// without limit on the request path.
		if lookups++; lookups > maxGrantSourceLookups {
			break
		}
		source, err := toolkits.Get(ctx, project, id)
		if err != nil {
			if isToolkitAbsent(err) {
				continue
			}
			return 0, nil, toolkitError(err, id)
		}
		if _, known := rw.Expander.Kinds[strings.ToLower(source.Type)]; known &&
			allowed(rw.Expander.Allowed, source.Type) {
			sources = append(sources, id)
		}
	}
	return owner, sources, nil
}

// maxGrantSourceLookups bounds the source toolkits one grant considers. An
// Inventory toolkit lists a handful of sources; the cap is far above any real
// list and only stops an unbounded one from costing one query each.
const maxGrantSourceLookups = 64

// isToolkitAbsent is toolkitError's refusal half: a listed source that no
// longer exists is skipped, not a failure.
func isToolkitAbsent(err error) bool {
	return errors.Is(err, repos.ErrCurrentToolkitNotFound) ||
		errors.Is(err, repos.ErrInvalidCurrentToolkitRequest)
}

// sourceToolBodyLimit bounds what the gate reads to decide. test_tool's own
// bound (toolkitrun.MaxRequestBodyBytes, restated so this package does not
// import the route): a larger body is refused by the handler anyway.
const sourceToolBodyLimit = 1 << 20

// sourceToolGrantTimeout bounds the grant lookup on the request path.
const sourceToolGrantTimeout = 2 * time.Second

// SourceToolGate wraps one test_tool route. A request the provider's grant
// admits (sourceToolAdmitted) passes through `execute`; every other request
// through `standard`, exactly as before. Both are the route's permission
// middlewares; this gate only CHOOSES between them, it never skips one.
func SourceToolGate(
	grants SourceToolGrants,
	provider, tool string,
	readOnly func(string) bool,
	standard, execute func(http.Handler) http.Handler,
	logger *slog.Logger,
) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		viaStandard, viaExecute := standard(next), execute(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if grants == nil || readOnly == nil || r.Method != http.MethodPost {
				viaStandard.ServeHTTP(w, r)
				return
			}
			if sourceToolAdmitted(r, grants, provider, tool, readOnly, logger) {
				viaExecute.ServeHTTP(w, r)
				return
			}
			viaStandard.ServeHTTP(w, r)
		})
	}
}

// sourceToolAdmitted decides, restoring the body it read whatever it decides.
func sourceToolAdmitted(
	r *http.Request, grants SourceToolGrants, provider, tool string,
	readOnly func(string) bool, logger *slog.Logger,
) bool {
	user, ok := auth.UserFromContext(r.Context())
	// The AUTHENTICATING token, and only a plain token principal: a native
	// client credential carries its anchor's id here, and a session none.
	if !ok || user.TokenID == "" || user.NativeClientID != "" {
		return false
	}
	userID, ok := user.OwningUserID()
	if !ok {
		return false
	}
	tokenID, tokenErr := strconv.ParseInt(user.TokenID, 10, 64)
	projectID, projectErr := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	toolkitID, toolkitErr := strconv.ParseInt(chi.URLParam(r, "toolID"), 10, 64)
	if tokenErr != nil || projectErr != nil || toolkitErr != nil {
		return false
	}
	if user.TokenProjectID != nil && *user.TokenProjectID != projectID {
		return false
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, sourceToolBodyLimit+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), r.Body), r.Body}
	if err != nil || len(raw) > sourceToolBodyLimit {
		return false
	}
	name, ok := engineSourceToolName(raw)
	if !ok {
		return false
	}
	if name == "" || !readOnly(name) {
		return false
	}

	ctx, cancel := context.WithTimeout(r.Context(), sourceToolGrantTimeout)
	defer cancel()
	admitted, err := grants.AdmitsSourceTool(ctx, projectID, userID, tokenID, toolkitID, provider, tool)
	if err != nil {
		logger.Warn("source tool grant lookup failed; the ordinary gate applies",
			"provider", provider, "project", projectID, "error", err)
		return false
	}
	return admitted
}

// engineSourceKeys are the top-level keys the Inventory engine sends to
// test_tool (services/elitea-inventory-engine/src/native.rs source_caller):
// an ALLOW-list, not a deny-list of toolkitrun.Body's fields. Any other
// key — llm_model, llm_settings, llm_configuration, mcp_tokens,
// mcp_authorization_reference, a field toolkitrun.Body gains later, or a
// case variant encoding/json would fold onto one of them — carries a choice
// (a model, a credential, an authorization) that must not ride on a
// borrowed permission, so the grant does not apply.
var engineSourceKeys = map[string]bool{
	"request_id": true, "tool_name": true, "tool_params": true, "toolkit_config": true,
}

// engineSourceToolName is the trimmed tool name of a body in exactly the
// engine's shape, or false: top-level keys within engineSourceKeys, matched
// exactly (no case folding), and toolkit_config, when present, an object
// holding at most toolkit_id. test_tool decodes the same bytes with
// encoding/json (last key wins, as here), so the name checked is the name
// that runs.
func engineSourceToolName(raw []byte) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", false
	}
	for key := range fields {
		if !engineSourceKeys[key] {
			return "", false
		}
	}
	if config, present := fields["toolkit_config"]; present && !IsNull(config) {
		var inner map[string]json.RawMessage
		if json.Unmarshal(config, &inner) != nil || inner == nil {
			return "", false
		}
		for key := range inner {
			if key != "toolkit_id" {
				return "", false
			}
		}
	}
	var name string
	if json.Unmarshal(fields["tool_name"], &name) != nil {
		return "", false
	}
	return strings.TrimSpace(name), true
}

// The Inventory engine's read-only rule for a source tool (Python
// `_filter_read_only_tools`; the Rust engine's investigate::read_only over
// READ_ONLY_PREFIXES / WRITE_OPERATION_PATTERNS in
// libs/rust/inventory-core/assets/python_inventory.json). Restated
// here so the platform enforces what the engine only offers;
// TestReadOnlyRulesMatchTheEngineAsset pins the two together.
var (
	ReadOnlyToolPrefixes = []string{
		"get_", "list_", "search_", "read_", "fetch_", "find_", "query_", "describe_",
		"show_", "view_", "retrieve_", "check_", "verify_", "validate_", "lookup_", "browse_",
	}
	WriteOperationPatterns = []string{
		"create", "update", "delete", "write", "post", "put", "remove", "add_", "set_",
		"modify", "edit", "insert", "patch", "upload", "send_", "submit", "publish",
		"assign", "unassign", "approve", "reject", "close_", "open_", "merge", "fork",
		"clone", "push", "commit", "branch", "tag",
	}
)

// ReadOnlySourceTool applies that rule: a read prefix, and no write pattern
// anywhere in the name.
func ReadOnlySourceTool(name string) bool {
	lowered := strings.ToLower(name)
	return slices.ContainsFunc(ReadOnlyToolPrefixes, func(p string) bool {
		return strings.HasPrefix(lowered, p)
	}) && !slices.ContainsFunc(WriteOperationPatterns, func(w string) bool {
		return strings.Contains(lowered, w)
	})
}
