// This file gates the edge identity-header strip (#368, guarding the fix from
// #326).
//
// elitea-main USED TO accept X-Auth-Type + X-Auth-ID as a finished identity from
// any address inside auth.form.yml's trusted_proxy_cidrs, which is the whole
// compose network. The browser edge sits on that network. Without the
// `strip-client-identity` middleware a caller with no credential at all chose
// its own user id, on every route that did not carry the #302 membership gate.
// Measured on a real stack before the fix: `curl -H 'X-Auth-ID: 4'` returned
// that user's record with HTTP 200.
//
// That reader is gone (#1359), and elitea-main strips the same names itself
// (apimw.StripClientIdentityHeaders; TestInProcessStripCoversTheEdgeFloor in
// edge_auth_target_test.go holds the two lists together). The cluster edge
// strip (the HTTPRoute) is kept as defence in depth (edge_gateway_api_test.go).
//
// WHERE THE STRIP RUNS NOW (#1359 P2).
//
// The compose stacks ran a Traefik with a `strip-client-identity` middleware,
// and this gate parsed it: the definition had to delete every required name
// and every router reaching elitea-main had to name it. That Traefik is gone.
// elitea-main is the compose edge, and the strip is its own root middleware.
// So the gate now sends the forged headers THROUGH the compose edge — the real
// router with the web relay on, on a real listener — and asserts what a route
// inside elitea-main receives. The three edits the YAML gate caught (dropping
// the middleware from a router, deleting it, shortening its list) map onto
// unmounting apimw.StripClientIdentityHeaders, moving the relay so it bypasses
// it, and shortening apimw.ClientIdentityHeaders; each one fails here.
//
// The runtime proof against a live stack is unchanged:
// deploy/scripts/standalone-stack.sh `check` forges a header against a live
// stack and requires a 401/403, requires a real personal access token on the
// same route to answer 200 with its OWN user, and requires a forged header not
// to override a genuine bearer. apps/elitea-web/scripts/chat-stream-e2e.sh runs
// that step in the CI — Web E2E workflow.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go.
package deployedge_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
)

// protectedService is the backend name of the process that receives these
// headers, as the Gateway API manifests name it.
const protectedService = "elitea-main"

// requiredStrippedHeaders is the floor, not the ceiling: each name here must be
// deleted, and a file may delete more. It stops a silent narrowing of the list.
//
// The X-Auth-* names are the retired forward-auth projection (#1359), which
// nothing reads any more. The X-Elitea-* names are the elitea-main to gateway
// identity projection, never a browser input; they are listed so that a future
// route through either edge cannot inherit a client-chosen project or tenant.
//
// ADD A NAME HERE when a header carries identity, authentication, or any other
// claim elitea-main or the gateway would BELIEVE. See mustForwardHeaders below
// for the one X-Elitea-* name that is deliberately NOT in this list, and the
// test that keeps it out.
var requiredStrippedHeaders = []string{
	"X-Auth-Type",
	"X-Auth-ID",
	"X-Auth-User-ID",
	"X-Auth-Reference",
	// The detached signature elitea-main requires beside the X-Auth-*
	// projection. Only the auth edge's own response may supply it.
	"X-Auth-Signature",
	"X-Auth-Avatar",
	"X-Auth-Avatar-State",
	"X-Auth-Session-Id",
	"X-Auth-Session-Name",
	"X-Auth-Session-Endpoint",
	"X-Elitea-Identity-Signature",
	"X-Elitea-Project-Id",
	"X-Elitea-User-Id",
	"X-Elitea-Tenant-Id",
	// The runtime execution id (shared migration 0100). It is legitimate input
	// from the RUNTIME WORKER, which does not pass this edge, and never from a
	// browser — and elitea-main re-signs whatever survives into the v2 identity
	// tuple, so a value left in place here would be attributed to an agent the
	// caller chose rather than to the execution that made the call.
	"X-Elitea-Execution-Id",
}

// hopMarkerHeader is the LLM gateway's hop marker (issue #164). The gateway
// stamps it on every outbound provider request and refuses any inbound request
// that already carries this deployment's own value, which contains a circular
// route on its first re-entry.
//
// The literal is repeated here on purpose. elitea-llm-gateway is a SEPARATE Go
// module (deliberately outside go.work), so this module cannot import its
// constant. Its internal/hopmarker/path_pin_test.go reads THIS file and fails
// if the two names drift apart, which is what stops the gateway renaming the
// header and leaving this gate protecting a name nothing sets any more.
const hopMarkerHeader = "X-Elitea-Llm-Hop"

// mustForwardHeaders are the names the browser edges must NOT delete. It is the
// exact inverse of requiredStrippedHeaders and, today, it holds one name.
//
// WHY THE HOP MARKER IS EXEMPT WHEN EVERY OTHER X-Elitea-* NAME IS STRIPPED.
//
// The canonical circular route leaves the gateway as an ordinary provider call
// and comes back in through this very edge:
//
//	gateway → provider (= the platform's own /llm) → EDGE → elitea-main → gateway
//
// The marker is the only thing that distinguishes that request from a first
// visit. An edge that deleted it would leave the gateway stamping every
// outbound request and never recognising one coming back — hop detection armed
// in the code and dead on the wire, with every unit test still green. Issue
// #12 established that the remaining layer cannot cover for it: the
// per-(project, model) breaker counts requests and does no hop detection at
// all, and no rate threshold there can separate a loop from ordinary traffic.
//
// The exemption is safe because the marker is NOT identity, which is the
// property every other name on the strip list has:
//
//   - It grants nothing. It names no user, project or tenant, and no code path
//     reads a permission, a credential or a budget from it.
//   - Recognising it REFUSES a request; it never admits one. The strip list
//     exists because elitea-main BELIEVES X-Auth-ID and X-Elitea-Project-Id, so
//     a client that chooses them chooses its own identity. Nothing is believed
//     here.
//   - It cannot be turned against anybody else. Detection reads one header of
//     one request, compares it in constant time and records nothing — no
//     counter, no circuit, no shared state — so a client that sends a harvested
//     marker gets its own request refused and reaches no other request,
//     project or tenant. (That statelessness is load-bearing: the marker is
//     transmitted to every upstream, so a marker that opened a shared circuit
//     would become a cross-tenant denial of service.)
//
// KEEP THIS LIST TINY. A name belongs here only if the same three statements
// are true of it.
var mustForwardHeaders = []string{
	hopMarkerHeader,
}

// executionHeader is the one floor name elitea-main keeps on purpose; see
// apimw.StripClientIdentityHeaders and TestInProcessStripCoversTheEdgeFloor.
const executionHeader = "X-Elitea-Execution-Id"

// identityProbePath is a route inside elitea-main that records the headers it
// receives. CurrentAgentStart is mounted on the root router behind every
// cross-cutting middleware, so what it sees is what any route sees.
const identityProbePath = "/api/v2/elitea_core/messages/prompt_lib/2/conversation"

func startIdentityProbeEdge(t *testing.T) (*composeEdge, *http.Header) {
	t.Helper()
	seen := new(http.Header)
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	})
	return startComposeEdge(t, api.RouterConfig{CurrentAgentStart: probe}, browserRemoteAddr), seen
}

// TestBrowserEdgesForwardTheHopMarker is the second half of the same gate, and
// it points the OPPOSITE way: it fails when the edge deletes a name that has
// to survive (issue #164).
//
// Two edits disarm the LLM gateway's loop detection, and neither one breaks
// any other test in either module:
//
//  1. adding the hop marker to apimw.ClientIdentityHeaders — it reads as
//     tidying up, because every other X-Elitea-* name belongs there;
//  2. adding it to requiredStrippedHeaders above, which then FORCES edit 1 on
//     whoever next runs TestInProcessStripCoversTheEdgeFloor.
//
// After either, the gateway keeps stamping the marker on every outbound
// provider request and never sees one come back. See mustForwardHeaders for
// why this one name is exempt from the rule the rest of the file enforces.
func TestBrowserEdgesForwardTheHopMarker(t *testing.T) {
	// 0. The floor on this gate's OWN input. An empty mustForwardHeaders makes
	//    every loop below iterate zero times.
	if len(mustForwardHeaders) == 0 {
		t.Fatal("mustForwardHeaders is empty, so this gate checked nothing.\n" +
			"It must name the LLM gateway's hop marker: an edge that deletes that header disarms " +
			"loop detection completely, with every unit test in both modules still green.")
	}
	if !containsFold(mustForwardHeaders, hopMarkerHeader) {
		t.Fatalf("mustForwardHeaders does not name %s, so the browser edges are free to delete it "+
			"and the LLM gateway can no longer recognise its own traffic coming back.", hopMarkerHeader)
	}

	// 1. The two lists must stay disjoint.
	for _, forwarded := range mustForwardHeaders {
		if containsFold(requiredStrippedHeaders, forwarded) || containsFold(apimw.ClientIdentityHeaders, forwarded) {
			t.Errorf(
				"%s is required to be stripped and required to survive.\n"+
					"Deleting it disarms the LLM gateway's hop detection; keeping it is what lets a "+
					"circular route be recognised on its first re-entry. Decide which, and say why "+
					"beside the list you change.",
				forwarded,
			)
		}
	}

	// 2. Through the compose edge, it must arrive untouched.
	edge, seen := startIdentityProbeEdge(t)
	header := http.Header{}
	for _, forwarded := range mustForwardHeaders {
		header.Set(forwarded, "marker-value")
	}
	answer := edge.do(t, http.MethodPost, identityProbePath, header)
	if answer.status != http.StatusAccepted || *seen == nil {
		t.Fatalf("status = %d: the probe route was not reached, so this gate proved nothing", answer.status)
	}
	for _, forwarded := range mustForwardHeaders {
		if got := seen.Get(forwarded); got != "marker-value" {
			t.Errorf(
				"%s reached elitea-main as %q through the compose edge.\n"+
					"This header must reach elitea-main untouched. The LLM gateway stamps it on every "+
					"outbound provider request and recognises it coming back, which is how a circular "+
					"route (gateway, provider = this platform's own /llm, this edge, elitea-main, "+
					"gateway) is contained on its first re-entry.",
				forwarded, got,
			)
		}
	}
}

// TestBrowserEdgesStripClientIdentity is the gate.
func TestBrowserEdgesStripClientIdentity(t *testing.T) {
	if len(requiredStrippedHeaders) == 0 {
		t.Fatal("requiredStrippedHeaders is empty, so this gate checked nothing.")
	}
	edge, seen := startIdentityProbeEdge(t)
	header := http.Header{}
	for _, name := range requiredStrippedHeaders {
		header.Set(name, "client-chosen")
	}
	answer := edge.do(t, http.MethodPost, identityProbePath, header)
	if answer.status != http.StatusAccepted || *seen == nil {
		t.Fatalf("status = %d: the probe route was not reached, so this gate proved nothing", answer.status)
	}
	for _, name := range requiredStrippedHeaders {
		if strings.EqualFold(name, executionHeader) {
			// Kept on purpose and verified on /llm; see the constant.
			continue
		}
		if got := seen.Get(name); got != "" {
			t.Errorf(
				"%s reached elitea-main as %q through the compose edge.\n"+
					"A name left in place is one a client chooses for itself (#326). Keep "+
					"apimw.StripClientIdentityHeaders mounted ahead of every route, and the relay "+
					"(internal/api/webedge) sending nothing but web paths to the web container.",
				name, got,
			)
		}
	}
}

// TestWebRelayCarriesNoClientIdentity: the relay to elitea-web (/app, /docs)
// is a route like any other, so the identity strip must run before it. It was
// mounted after the relay once, and every /app and /docs request reached the
// web container with the caller's X-Auth-* and X-Elitea-* names intact.
func TestWebRelayCarriesNoClientIdentity(t *testing.T) {
	if len(requiredStrippedHeaders) == 0 {
		t.Fatal("requiredStrippedHeaders is empty, so this gate checked nothing.")
	}
	edge, _ := startIdentityProbeEdge(t)
	header := http.Header{}
	for _, name := range requiredStrippedHeaders {
		header.Set(name, "client-chosen")
	}
	for _, path := range []string{"/app/", "/app/assets/index.js", "/docs/", "/docs/deployment/argocd"} {
		answer := edge.do(t, http.MethodGet, path, header)
		if !answer.reachedWeb {
			t.Fatalf("%s did not reach the web container (status %d), so this gate proved nothing", path, answer.status)
		}
		for _, name := range requiredStrippedHeaders {
			if strings.EqualFold(name, executionHeader) {
				continue // kept on purpose by the strip itself; see the constant
			}
			if got := answer.web.header.Get(name); got != "" {
				t.Errorf("%s reached elitea-web on %s as %q: mount apimw.StripClientIdentityHeaders ahead of webedge.Middleware", name, path, got)
			}
		}
	}
}
