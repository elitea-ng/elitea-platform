package middleware

import "net/http"

// ClientIdentityHeaders are the request headers StripClientIdentityHeaders
// deletes. It is the list the cluster edge carries as the HTTPRoute's
// RequestHeaderModifier (deploy/gateway-api/httproute.yaml), less one name;
// see StripClientIdentityHeaders. In a compose stack elitea-main is the browser
// edge itself (internal/api/webedge), so this list IS that edge's strip.
//
// Extend it; never shorten it. tests/deployedge holds it to those edge lists.
var ClientIdentityHeaders = []string{
	// The X-Auth-* family the retired forward-auth projection used (#1359).
	// Nothing in elitea-main reads them any more; deleting them anyway keeps
	// a later reader from inheriting a caller-chosen value.
	"X-Auth-Type",
	"X-Auth-ID",
	"X-Auth-User-ID",
	"X-Auth-Reference",
	"X-Auth-Signature",
	"X-Auth-Avatar",
	"X-Auth-Avatar-State",
	"X-Auth-Session-Id",
	"X-Auth-Session-Name",
	"X-Auth-Session-Endpoint",
	// elitea-main's signed projection to the LLM gateway and the sub-app
	// hosts (internal/llmproxy/identity.go). elitea-main WRITES these on its
	// outbound calls; no caller of elitea-main ever legitimately sends one.
	"X-Elitea-Identity-Signature",
	"X-Elitea-Project-Id",
	"X-Elitea-User-Id",
	"X-Elitea-Tenant-Id",
}

// StripClientIdentityHeaders deletes every ClientIdentityHeaders name from the
// inbound request before any route runs.
//
// It used to be an edge's job, and elitea-main was only as safe as the edge in
// front of it: a deployment that published elitea-main behind a plain ingress
// forwarded whatever the client sent. Done here, the guarantee holds on every
// listener this router serves, whatever is in front of it.
//
// X-Elitea-Execution-Id IS DELIBERATELY KEPT, although the browser edges
// deleted it. It is a legitimate CLIENT input on /llm: the desktop's local
// turns send it on the public listener (application/localturn, client
// contract 1.5) and the runtime worker sends it on its own model calls
// (libs/rust/llm-wire EXECUTION_HEADER). It is never trusted as sent: the /llm
// edge keeps it only for a live execution of the same project and caller
// (llmproxy.Config.ExecutionVerifier), and a runtime claim on the artifact
// routes must also prove the claim's fence token (v2/artifacts
// RuntimeClaimHeaders). Deleting it here would silently drop the desktop's
// cost attribution, which is what the browser edges did.
func StripClientIdentityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range ClientIdentityHeaders {
			r.Header.Del(name)
		}
		next.ServeHTTP(w, r)
	})
}
