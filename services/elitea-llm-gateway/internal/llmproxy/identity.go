package llmproxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// Signed internal identity headers forwarded by elitea-main. The gateway trusts
// these only because the hop is mTLS-internal (design §2, §6.1); the HMAC
// signature is defense-in-depth so a header that leaks onto the internal
// network without the shared secret cannot forge a project identity. These
// mirror elitea-main/internal/llmproxy/identity.go (a separate module).
const (
	headerProjectID = "X-Elitea-Project-Id"
	headerUserID    = "X-Elitea-User-Id"
	headerTenantID  = "X-Elitea-Tenant-Id"
	// headerExecutionID names the runtime execution this request was made
	// FROM — the value elitea_runtime.execution_jobs is keyed by. It is what
	// gives the request log and the usage ledger an AGENT dimension, and it is
	// an execution id rather than an agent id on purpose: the runtime already
	// mints it, it is stable across retries of the same turn, and resolving it
	// to an agent at READ time keeps the two project columns on execution_jobs
	// out of the log (see elitea-main internal/infra/db/repos/analytics.go).
	//
	// It is part of the SIGNED tuple under v2. See signatureVersionV2.
	headerExecutionID = "X-Elitea-Execution-Id"
	// headerSignature carries "sha256=<hex>" over the canonical identity tuple.
	headerSignature = "X-Elitea-Identity-Signature"
)

// The canonical string is prefixed with a scheme VERSION so it can be rotated
// without ambiguity, and this is the rotation the prefix was put there for.
//
// v1 signs (project, user, tenant). v2 signs (project, user, tenant, execution).
//
// BOTH ARE ACCEPTED, and they have to be: the canonical string is duplicated
// across two independently deployed Go modules — this file and elitea-main's
// internal/llmproxy/identity.go — so during any rolling deploy an edge signing
// one scheme talks to a gateway verifying the other. Changing the string in
// place would have failed EVERY request in both directions for the length of
// the rollout.
//
// The edge signs v2 only when it actually has an execution id, and v1 (byte
// for byte what it signed before) otherwise. So existing traffic — chat, the
// SDK, every /llm caller that is not a runtime execution — keeps producing a
// signature an OLD gateway still accepts, and only the new agent-tagged
// requests require a new gateway. That makes the deploy order "gateway first"
// rather than a hard cutover.
//
// A v1 signature is accepted ONLY when no execution-id header is present. A v1
// canonical string does not cover the execution id, so accepting one alongside
// the header would let anything that could replay a captured v1 tuple attach an
// arbitrary execution id to it — and the whole point of putting the id in the
// signed tuple is that it cannot be attached by the caller.
const (
	signatureVersionV1 = "v1"
	signatureVersionV2 = "v2"
)

// identity is the resolved edge identity carried on an inbound gateway request.
// projectID is the Bifrost virtual-key value (a resolved, non-secret handle —
// never the raw Elitea key; design §2, §6.1).
type identity struct {
	projectID string
	userID    string
	tenantID  string
	// executionID is the runtime execution this request belongs to, empty for
	// every request that is not made from one.
	executionID string
}

// identityFromHeaders reads the forwarded identity headers off an inbound
// request. It does not validate the signature; callers use verifySignature for
// that.
func identityFromHeaders(h http.Header) identity {
	return identity{
		projectID:   h.Get(headerProjectID),
		userID:      h.Get(headerUserID),
		tenantID:    h.Get(headerTenantID),
		executionID: h.Get(headerExecutionID),
	}
}

// canonical returns the deterministic string signed by the HMAC under version.
// Newline separation with a version prefix prevents field-concatenation
// ambiguity (e.g. project "1"+user "2" colliding with project "12"). MUST match
// the edge.
//
// The v1 string is UNCHANGED — it must stay byte-identical, because an edge
// that has not been redeployed is still producing signatures over it.
func (id identity) canonical(version string) string {
	base := version + "\n" + id.projectID + "\n" + id.userID + "\n" + id.tenantID
	if version == signatureVersionV2 {
		return base + "\n" + id.executionID
	}
	return base
}

// sign returns the "sha256=<hex>" signature of the identity under secret, using
// v2 when an execution id is present and v1 when it is not.
func (id identity) sign(secret []byte) string {
	return id.signVersion(secret, id.signatureVersion())
}

// signatureVersion is the scheme this identity signs under: v2 exactly when
// there is an execution id to cover, v1 otherwise.
func (id identity) signatureVersion() string {
	if id.executionID != "" {
		return signatureVersionV2
	}
	return signatureVersionV1
}

func (id identity) signVersion(secret []byte, version string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id.canonical(version)))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// verifySignature reports whether the identity headers on h carry a valid
// signature under secret. It fails closed: an EMPTY secret refuses every
// request, because there is nothing to verify against and the identity
// headers select a tenant's provider credentials and budget. There is no
// "verification disabled" mode; startupIdentityCheck refuses to start the
// gateway without a strong secret, so this branch only guards a Handler built
// without one. Returns false, too, when the signature header is absent or
// does not match.
//
// It accepts BOTH schemes during a rolling deploy, with the asymmetry the
// version block above states: v2 always, v1 only when the request carries no
// execution-id header.
func verifySignature(h http.Header, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	got := h.Get(headerSignature)
	if got == "" {
		return false
	}
	id := identityFromHeaders(h)
	if hmac.Equal([]byte(got), []byte(id.signVersion(secret, signatureVersionV2))) {
		return true
	}
	if id.executionID != "" {
		// A v1 signature does not cover the execution id. Falling back here
		// would make the id caller-attachable, which is exactly what signing it
		// is for.
		return false
	}
	return hmac.Equal([]byte(got), []byte(id.signVersion(secret, signatureVersionV1)))
}

// SignIdentityHeaders sets the identity headers and HMAC signature on h so
// verifySignature will accept the request. Non-empty projectID, userID,
// tenantID and executionID are set on their respective headers; an empty value
// omits the header. An empty secret sets no signature header, and the
// gateway then refuses the request.
//
// Exported for test-support packages that must forge a signed request without
// going through the real edge (e.g. internal/preflight).
func SignIdentityHeaders(h http.Header, secret []byte, projectID, userID, tenantID, executionID string) {
	if projectID != "" {
		h.Set(headerProjectID, projectID)
	}
	if userID != "" {
		h.Set(headerUserID, userID)
	}
	if tenantID != "" {
		h.Set(headerTenantID, tenantID)
	}
	if executionID != "" {
		h.Set(headerExecutionID, executionID)
	}
	if len(secret) > 0 {
		id := identity{
			projectID:   projectID,
			userID:      userID,
			tenantID:    tenantID,
			executionID: executionID,
		}
		h.Set(headerSignature, id.sign(secret))
	}
}
