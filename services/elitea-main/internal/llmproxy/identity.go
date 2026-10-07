// Package llmproxy is elitea-main's lightweight /llm gateway role: a streaming
// reverse proxy that authenticates at the edge, resolves the caller's project,
// injects signed identity headers, and byte-streams /llm to
// elitea-llm-gateway-svc over mTLS (design §2, ADR-0015 Option E). It does NOT
// import bifrost/core, translate wire dialects, or run governance.
package llmproxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

// Signed internal identity headers forwarded to the gateway. The gateway trusts
// these only because the hop is mTLS-internal (design §2, §6.1); the HMAC
// signature is defense-in-depth so a header that leaks onto the internal network
// without the shared secret cannot forge a project identity.
const (
	HeaderProjectID = "X-Elitea-Project-Id"
	HeaderUserID    = "X-Elitea-User-Id"
	HeaderTenantID  = "X-Elitea-Tenant-Id"
	// HeaderExecutionID names the runtime execution a /llm request was made
	// FROM — the value elitea_runtime.execution_jobs is keyed by. It is what
	// gives the gateway's request log and usage ledger an AGENT dimension.
	//
	// It is an EXECUTION id and not an agent id on purpose: the runtime already
	// mints it, it is stable across retries of the same turn, and resolving it
	// to an agent at READ time keeps execution_jobs' two project columns —
	// resource_project_id and projection_project_id, which can differ — out of
	// the log. Copying an agent id onto the log would copy that ambiguity with
	// it (internal/infra/db/repos/analytics.go).
	//
	// Unlike the three headers above it is not resolved from this process's own
	// middleware: the caller that knows it is the runtime worker, which sends
	// it inbound. The edge therefore READS it off the inbound request, validates
	// its shape, checks that it names a live execution of the caller
	// (ExecutionVerifier), and re-emits it as part of the signed tuple — see
	// executionIDFromHeader.
	HeaderExecutionID = "X-Elitea-Execution-Id"
	// HeaderSignature carries "sha256=<hex>" over the canonical identity tuple.
	HeaderSignature = "X-Elitea-Identity-Signature"
)

// The canonical string is prefixed with a scheme VERSION so it can be rotated
// without ambiguity, and this is the rotation the prefix was put there for.
//
// v1 signs (project, user, tenant). v2 signs (project, user, tenant, execution).
//
// THE CANONICAL STRING IS DUPLICATED ACROSS TWO INDEPENDENTLY DEPLOYED GO
// MODULES — this file signs it and services/elitea-llm-gateway's
// internal/llmproxy/identity.go verifies it. Changing it in place would fail
// EVERY gateway request in both directions for the length of a rolling deploy.
//
// So this edge signs v2 only when it actually has an execution id, and v1 —
// byte for byte what it signed before — otherwise. Every existing caller (chat,
// the SDK, anything that is not a runtime execution) therefore keeps producing
// a signature an OLD gateway still accepts, and only the new agent-tagged
// requests need a new gateway. The deploy order is "gateway first" rather than
// a hard cutover, and no request is signed under a scheme its peer cannot at
// least recognise.
const (
	signatureVersionV1 = "v1"
	signatureVersionV2 = "v2"
)

// maxExecutionIDLen bounds the inbound execution id.
//
// The value is minted by the runtime and is a short opaque token; the bound
// exists because this one field arrives on the request rather than being
// resolved from the session, so it is the only part of the signed tuple a
// caller can put bytes into. See executionIDFromHeader for what it may contain
// and why an invalid value is DROPPED rather than refused.
const maxExecutionIDLen = 128

// identity is the resolved edge identity forwarded to the gateway. The
// projectID is the resolved, non-secret handle used as the Bifrost virtual-key
// value at the gateway — never the raw Elitea key (design §2, §6.1).
type identity struct {
	projectID string
	userID    string
	tenantID  string
	// executionID is the runtime execution this request belongs to, empty for
	// every request that is not made from one.
	executionID string
}

// identityFromContext derives the forwarded identity from the request context
// populated by the auth, project-resolution, and tenant middleware. Any field
// that cannot be resolved is left empty; the proxy still forwards, and the
// gateway decides whether a missing project is fatal (IsVkMandatory).
func identityFromContext(ctx context.Context) identity {
	var id identity

	if pc, ok := middleware.ProjectFromContext(ctx); ok && pc.ProjectID > 0 {
		id.projectID = strconv.Itoa(pc.ProjectID)
	}
	if u, ok := auth.UserFromContext(ctx); ok {
		id.userID = u.ID
	}
	if t, ok := tenant.TenantFromContext(ctx); ok {
		id.tenantID = t
	}
	return id
}

// canonical returns the deterministic string signed by the HMAC. Newline
// separation with a version prefix prevents field-concatenation ambiguity
// (e.g. project "1"+user "2" colliding with project "12").
func (id identity) canonical(version string) string {
	base := version + "\n" + id.projectID + "\n" + id.userID + "\n" + id.tenantID
	if version == signatureVersionV2 {
		return base + "\n" + id.executionID
	}
	return base
}

// signatureVersion is the scheme this identity signs under: v2 exactly when
// there is an execution id to cover, v1 otherwise.
func (id identity) signatureVersion() string {
	if id.executionID != "" {
		return signatureVersionV2
	}
	return signatureVersionV1
}

// sign returns the "sha256=<hex>" signature of the identity under secret.
func (id identity) sign(secret []byte) string {
	return id.signVersion(secret, id.signatureVersion())
}

func (id identity) signVersion(secret []byte, version string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id.canonical(version)))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// executionIDFromHeader reads and validates the SHAPE of the inbound
// execution id.
//
// An INVALID value yields "", which drops the dimension for that request rather
// than refusing it. A malformed id costs a row in a breakdown, while refusing
// would let a bad header break the product's most visible path.
//
// The charset is restrictive because the value is re-emitted as a HEADER and
// then signed: a CR or LF in it would be header injection on the outbound
// request, and nothing outside this set belongs in an id the runtime minted.
//
// A `:` is refused although the log column allows it. Ids with a `:` are
// reserved for attributions elitea-main signs IN PROCESS: the evaluation
// orchestrator's `eval:<run>:case:<case>` and `eval:<run>:judge:<case>`
// (internal/api/v2/evaluation/attribution.go). Those calls go to the gateway
// directly and never through this edge. A runtime execution id is a hex or
// UUID token with no `:`. So an inbound value with a `:` names an evaluation
// run, and the analytics reads would add the caller's spend to that run.
//
// The shape is not enough. The analytics reads DECIDE things from this id:
// the trigger origin of the execution it names removes a call from every
// active-user figure, and the per-execution read adds the call to that run's
// spend. So injectIdentity also asks an ExecutionVerifier, when one is set,
// whether the id names a live execution of the caller.
func executionIDFromHeader(h http.Header) string {
	value := h.Get(HeaderExecutionID)
	if value == "" || len(value) > maxExecutionIDLen {
		return ""
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return value
}

// ExecutionVerifier decides whether a caller may attribute a /llm call to an
// execution id.
//
// The runtime worker calls /llm with the execution actor's own access token,
// so the edge cannot tell the worker from that person's SDK client by the
// credential. What it can check is the execution: the id must name an
// execution of the resolved project, that execution must run as the caller,
// and it must still be live (not settled, or settled moments ago). A person
// can then attribute calls only to their own run, while it runs.
//
// VerifyExecution returns false, nil for an id that fails the rule. An error
// is a failed lookup, and the edge drops the id for it too: attribution is
// never worth failing a model call for.
type ExecutionVerifier interface {
	VerifyExecution(ctx context.Context, projectID, userID, executionID string) (bool, error)
}

// CallbackExecutionPrefix marks an execution id that names a provider
// invocation rather than a runtime execution: `callback-<token uuid>`, where
// the uuid is the callback token minted for that one invocation
// (providerhost/material.CallbackSettings). A runtime execution id is 32 hex
// characters, so the two never collide.
const CallbackExecutionPrefix = "callback-"

// CallbackExecutionVerifier is the optional half of a verifier that also
// admits provider invocations. A provider engine (DeepWiki, Inventory) calls
// /llm with the callback token minted for its invocation; there is no
// execution_jobs row for that work, so the token IS the execution. The rule:
// the id names the very token that authenticated this request (tokenID, the
// principal's auth_core__token id), owned by userID, bound to projectID, and
// not expired. Only the holder of that bearer can claim the id, and only
// while the bearer lives.
type CallbackExecutionVerifier interface {
	VerifyCallbackExecution(ctx context.Context, projectID, userID, tokenID, tokenUUID string) (bool, error)
}

// executionVerifyTimeout bounds the lookup on the request path.
const executionVerifyTimeout = 2 * time.Second

// verifiedExecutionID applies verifier to executionID. A nil verifier keeps
// the shape check only, which is the behaviour of a deployment with no
// database behind the edge.
func verifiedExecutionID(ctx context.Context, verifier ExecutionVerifier, id identity, executionID string) string {
	if executionID == "" || verifier == nil {
		return executionID
	}
	if id.projectID == "" || id.userID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, executionVerifyTimeout)
	defer cancel()
	if tokenUUID, ok := strings.CutPrefix(executionID, CallbackExecutionPrefix); ok {
		callbacks, implemented := verifier.(CallbackExecutionVerifier)
		user, authenticated := auth.UserFromContext(ctx)
		if !implemented || !authenticated || user.TokenID == "" || tokenUUID == "" {
			return ""
		}
		ok, err := callbacks.VerifyCallbackExecution(ctx, id.projectID, id.userID, user.TokenID, tokenUUID)
		if err != nil || !ok {
			return ""
		}
		return executionID
	}
	ok, err := verifier.VerifyExecution(ctx, id.projectID, id.userID, executionID)
	if err != nil || !ok {
		return ""
	}
	return executionID
}

// stripIdentityHeaders removes any caller-supplied authentication and identity
// headers before the request is forwarded to the gateway. The gateway must only
// ever see the signed X-Elitea-* identity that this edge process injects; it
// must never see client-supplied authentication material.
//
// Stripped headers:
//   - X-Elitea-* (signed identity injected below; strip first to avoid leaking
//     any client-spoofed value)
//   - X-Auth-Type / X-Auth-Id / X-Auth-Reference (Traefik edge-auth headers)
//   - Authorization, X-Api-Key (bearer / API-key credentials)
//   - Cookie (session cookies; must not reach the downstream gateway)
//   - X-Project-Id / OpenAI-Organization (the edge project selector; the edge
//     consumes it and speaks the result as signed identity)
//   - OpenAI-Project (never a selector, but it must not travel onward either)
func stripIdentityHeaders(h http.Header) {
	// Signed edge identity headers — re-injected after stripping.
	h.Del(HeaderProjectID)
	h.Del(HeaderUserID)
	h.Del(HeaderTenantID)
	h.Del(HeaderExecutionID)
	h.Del(HeaderSignature)

	// The project headers are edge control headers (issue #318). The edge
	// already read the selector, checked the caller's membership, and expressed
	// the result as X-Elitea-Project-Id. Forwarding any of them would send an
	// Elitea project id onward under a name a real provider reads: OpenAI
	// rejects a request whose OpenAI-Organization names an organization the key
	// cannot use. OpenAI-Project is stripped as well, although the edge never
	// reads it as a selector, because it carries an Elitea project id too.
	for _, name := range middleware.ProjectHeadersStrippedOutbound() {
		h.Del(name)
	}

	// Traefik edge-auth headers that the auth middleware reads; remove so the
	// gateway never sees inbound authentication context.
	h.Del("X-Auth-Type")
	h.Del("X-Auth-Id")
	h.Del("X-Auth-Reference")

	// Standard HTTP authentication material.
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Cookie")
}

// injectIdentity strips any client-supplied identity headers on out, then sets
// the edge-resolved identity. When secret is non-empty and a project is present
// it also attaches the HMAC signature. verifier, when set, decides whether the
// inbound execution id is kept (ExecutionVerifier).
func injectIdentity(ctx context.Context, out http.Header, secret []byte, verifier ExecutionVerifier) {
	// READ BEFORE STRIP. stripIdentityHeaders deletes every X-Elitea-* header,
	// this one included, and `out` is a copy of the inbound headers — so the
	// caller-supplied execution id has to be taken (and validated) here, before
	// the strip removes it. Taking it afterwards would silently produce an empty
	// dimension on every request: a column that is always NULL and a breakdown
	// that is always empty, with nothing failing.
	executionID := executionIDFromHeader(out)

	stripIdentityHeaders(out)

	id := identityFromContext(ctx)
	id.executionID = verifiedExecutionID(ctx, verifier, id, executionID)
	if id.projectID != "" {
		out.Set(HeaderProjectID, id.projectID)
	}
	if id.userID != "" {
		out.Set(HeaderUserID, id.userID)
	}
	if id.tenantID != "" {
		out.Set(HeaderTenantID, id.tenantID)
	}
	if id.executionID != "" {
		out.Set(HeaderExecutionID, id.executionID)
	}

	// Sign only when we have both a secret and a project identity to bind; an
	// unsigned request over the mTLS-internal network is still authenticated by
	// the transport, but a signature lets the gateway reject stray traffic.
	if len(secret) > 0 && id.projectID != "" {
		out.Set(HeaderSignature, id.sign(secret))
	}
}

// SignIdentityHeaders sets the signed X-Elitea-* identity headers on out from
// explicit project/user/tenant values, using the identical HMAC scheme
// injectIdentity uses. It exists for elitea-main callers that resolve their
// own project identity from something other than the request-scoped
// middleware context injectIdentity reads — e.g. the configurations
// check-connection client (internal/api/v2/configurations), whose project id
// is a URL path parameter, not a *middleware.ProjectContext the /configurations
// mount never populates. Mirrors the gateway's own exported
// SignIdentityHeaders (elitea-llm-gateway/internal/llmproxy/identity.go) so
// the two sides cannot silently diverge on the signing scheme.
func SignIdentityHeaders(out http.Header, secret []byte, projectID, userID, tenantID, executionID string) {
	stripIdentityHeaders(out)
	id := identity{projectID: projectID, userID: userID, tenantID: tenantID, executionID: executionID}
	if id.projectID != "" {
		out.Set(HeaderProjectID, id.projectID)
	}
	if id.userID != "" {
		out.Set(HeaderUserID, id.userID)
	}
	if id.tenantID != "" {
		out.Set(HeaderTenantID, id.tenantID)
	}
	if id.executionID != "" {
		out.Set(HeaderExecutionID, id.executionID)
	}
	if len(secret) > 0 && id.projectID != "" {
		out.Set(HeaderSignature, id.sign(secret))
	}
}

// verifyIdentitySignature reports whether the identity headers on h carry a
// valid signature under secret. It is used by tests here and is safe for the
// gateway side to mirror. Returns false if the signature header is absent.
func verifyIdentitySignature(h http.Header, secret []byte) bool {
	got := h.Get(HeaderSignature)
	if got == "" {
		return false
	}
	id := identity{
		projectID:   h.Get(HeaderProjectID),
		userID:      h.Get(HeaderUserID),
		tenantID:    h.Get(HeaderTenantID),
		executionID: h.Get(HeaderExecutionID),
	}
	// Mirrors the gateway's acceptance rule exactly: v2 always, v1 only when no
	// execution id is present. A v1 canonical string does not cover the id, so
	// accepting one beside the header would make the id caller-attachable —
	// the single thing signing it prevents.
	if hmac.Equal([]byte(got), []byte(id.signVersion(secret, signatureVersionV2))) {
		return true
	}
	if id.executionID != "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(id.signVersion(secret, signatureVersionV1)))
}
