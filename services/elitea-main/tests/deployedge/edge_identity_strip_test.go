// This file gates the edge identity-header strip (#368, guarding the fix from
// #326).
//
// elitea-main accepts X-Auth-Type + X-Auth-ID as a finished identity from any
// address inside auth.form.yml's trusted_proxy_cidrs, which is the whole compose
// network. The browser edge sits on that network. Without the
// `strip-client-identity` middleware a caller with no credential at all chooses
// its own user id, on every route that does not carry the #302 membership gate.
// Measured on a real stack before the fix: `curl -H 'X-Auth-ID: 4'` returned
// that user's record with HTTP 200.
//
// WHAT THIS GATE ADDS, AND WHAT IT DOES NOT DUPLICATE.
//
// TestEveryRouterMiddlewareResolves in edge_middlewares_test.go already fails
// when the DEFINITION disappears while a router still names it. Three edits it
// cannot see:
//
//	1. dropping `strip-client-identity` from a router's middlewares list;
//	2. deleting the reference and the definition together;
//	3. removing header names from the list, which leaves a middleware that
//	   still resolves and still strips almost everything.
//
// Each of the three restores the bypass. This gate fails on all three.
//
// The runtime proof is stronger and lives elsewhere:
// deploy/scripts/standalone-stack.sh `check` forges a header against a live
// stack and requires a 401/403, requires a real personal access token on the
// same route to answer 200 with its OWN user, and requires a forged header not
// to override a genuine bearer. apps/elitea-web/scripts/chat-stream-e2e.sh runs
// that step, so the CI — Web E2E workflow executes it on every pull request that
// touches deploy/** or services/elitea-main/**.
//
// That runtime proof reaches ONE of the two files: the chat-stream stack mounts
// dynamic.e2e.yml. No stack in continuous integration mounts dynamic.yml, so
// this static gate is what covers it. Both are checked here, and the two must
// agree — a fix in only one file leaves whichever stack mounts the other wide
// open, which is the #326 defect itself.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module.
package deployedge_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// stripMiddlewareName is the middleware every browser-edge router that reaches
// elitea-main must carry.
const stripMiddlewareName = "strip-client-identity"

// protectedService is the load-balancer name of the process that trusts these
// headers. Routers are selected by the service they reach and not by their own
// names, so a NEW router added to either file is covered from the day it is
// written.
const protectedService = "elitea-main"

// browserEdgeFiles are the two files #326 fixed. Both front elitea-main from a
// browser and NEITHER runs an edgeAuth, so no identity header arriving at
// them is ever legitimate client input.
//
// deploy/runtime/platform-edge-dynamic.yml is deliberately not in this list. It
// has a different shape: it authenticates with edgeAuth and then PROJECTS
// verified identity headers, so the same names are legitimate on the way out of
// its middleware chain. Holding it to the rule below would assert something
// that is not true of it.
var browserEdgeFiles = []string{
	"deploy/traefik/dynamic.yml",
	"deploy/traefik/dynamic.e2e.yml",
}

// requiredStrippedHeaders is the floor, not the ceiling: each name here must be
// deleted, and a file may delete more. It stops a silent narrowing of the list.
//
// The X-Auth-* names are what internal/api/middleware/auth.go reads through
// tryTrustedProxyHeaders. The X-Elitea-* names are the elitea-main to gateway
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

// edgeWithHeaders re-reads the edge YAML with the header fields this gate needs.
// dynamicConfig in edge_middlewares_test.go models middleware CHAINS and stops
// there; adding headers to it would change what that gate parses.
type edgeWithHeaders struct {
	HTTP struct {
		Routers map[string]struct {
			Service     string   `yaml:"service"`
			Middlewares []string `yaml:"middlewares"`
		} `yaml:"routers"`
		Middlewares map[string]struct {
			Headers struct {
				CustomRequestHeaders map[string]string `yaml:"customRequestHeaders"`
			} `yaml:"headers"`
		} `yaml:"middlewares"`
	} `yaml:"http"`
}

func parseEdgeWithHeaders(t *testing.T, path string) edgeWithHeaders {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed edgeWithHeaders
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as Traefik dynamic configuration: %v", path, err)
	}
	return parsed
}

// TestBrowserEdgesForwardTheHopMarker is the second half of the same gate, and
// it points the OPPOSITE way: it fails when an edge deletes a name that has to
// survive (issue #164).
//
// Two edits disarm the LLM gateway's loop detection, and neither one breaks any
// other test in either module:
//
//  1. adding the hop marker to strip-client-identity's customRequestHeaders —
//     it reads as tidying up, because every other X-Elitea-* name belongs
//     there;
//  2. adding it to requiredStrippedHeaders above, which then FORCES edit 1 on
//     whoever next runs this gate.
//
// After either, the gateway keeps stamping the marker on every outbound
// provider request and never sees one come back. Hop detection is armed in the
// code, dead on the wire, and the only remaining layer is the amplification
// backstop, which does no hop detection at all (issue #12).
//
// See mustForwardHeaders for why this one name is exempt from the rule the rest
// of the file enforces.
func TestBrowserEdgesForwardTheHopMarker(t *testing.T) {
	root := repoRoot(t)

	// 0. The floor on this gate's OWN input. An empty mustForwardHeaders makes
	//    every loop below iterate zero times and the whole gate report a clean
	//    tree, which is how a gate stops gating without anyone noticing.
	if len(mustForwardHeaders) == 0 {
		t.Fatal("mustForwardHeaders is empty, so this gate checked nothing.\n" +
			"It must name the LLM gateway's hop marker: an edge that deletes that header disarms " +
			"loop detection completely, with every unit test in both modules still green.")
	}
	found := false
	for _, name := range mustForwardHeaders {
		if name == hopMarkerHeader {
			found = true
		}
	}
	if !found {
		t.Fatalf("mustForwardHeaders does not name %s, so the browser edges are free to delete it "+
			"and the LLM gateway can no longer recognise its own traffic coming back.", hopMarkerHeader)
	}

	// 1. The two lists must stay disjoint. A name cannot be both required to
	//    go and required to stay, and this is where that contradiction shows
	//    up first — before anybody edits the YAML to satisfy it.
	for _, forwarded := range mustForwardHeaders {
		for _, stripped := range requiredStrippedHeaders {
			if !strings.EqualFold(forwarded, stripped) {
				continue
			}
			t.Errorf(
				"%s is in BOTH requiredStrippedHeaders and mustForwardHeaders.\n"+
					"Deleting it disarms the LLM gateway's hop detection; keeping it is what lets a "+
					"circular route be recognised on its first re-entry. Decide which, and say why "+
					"beside the list you change.",
				forwarded,
			)
		}
	}

	// 2. Neither browser edge may delete it. Traefik deletes a header whose
	//    customRequestHeaders value is empty, which is the exact shape the
	//    strip list uses, so presence AT ALL in that map is the failure.
	for _, relative := range browserEdgeFiles {
		absolute := filepath.Join(root, relative)
		if _, err := os.Stat(absolute); err != nil {
			t.Fatalf(
				"%s does not exist. The edge moved and this gate stopped gating. "+
					"Update browserEdgeFiles in this file.",
				relative,
			)
		}
		definition, defined := parseEdgeWithHeaders(t, absolute).HTTP.Middlewares[stripMiddlewareName]
		if !defined {
			// TestBrowserEdgesStripClientIdentity already reports this, and it
			// reports it as the more serious fault. Nothing to add here.
			continue
		}
		for _, forwarded := range mustForwardHeaders {
			value, present := definition.Headers.CustomRequestHeaders[forwarded]
			if !present {
				continue
			}
			t.Errorf(
				"%s: %q names %s (value %q).\n"+
					"This header must reach elitea-main untouched. The LLM gateway stamps it on every "+
					"outbound provider request and recognises it coming back, which is how a circular "+
					"route — gateway → provider (= this platform's own /llm) → this edge → elitea-main "+
					"→ gateway — is contained on its first re-entry. An edge that deletes it leaves the "+
					"gateway unable to recognise its own traffic, with every unit test still green.\n"+
					"It is not identity: it grants nothing, names no project, and the only thing a "+
					"client achieves by sending it is the refusal of its own request.",
				relative, stripMiddlewareName, forwarded, value,
			)
		}
	}
}

// TestBrowserEdgesStripClientIdentity is the gate.
func TestBrowserEdgesStripClientIdentity(t *testing.T) {
	root := repoRoot(t)

	// Keyed by file, so the cross-file comparison below reports which side moved.
	strippedPerFile := map[string][]string{}

	for _, relative := range browserEdgeFiles {
		absolute := filepath.Join(root, relative)
		if _, err := os.Stat(absolute); err != nil {
			t.Fatalf(
				"%s does not exist. The edge moved and this gate stopped "+
					"gating. Update browserEdgeFiles in this file.",
				relative,
			)
		}
		parsed := parseEdgeWithHeaders(t, absolute)

		// 1. The middleware must be defined, and it must delete every required
		//    name. Traefik deletes a header whose customRequestHeaders value is
		//    empty; a NON-empty value would set the header instead, which is the
		//    opposite of the control.
		definition, defined := parsed.HTTP.Middlewares[stripMiddlewareName]
		if !defined {
			t.Errorf(
				"%s defines no %q middleware, so this edge forwards whatever "+
					"identity headers the client sent. elitea-main trusts "+
					"X-Auth-Type + X-Auth-ID from any trusted-proxy address, "+
					"and this edge is one, so an unauthenticated caller picks "+
					"its own user id (#326).",
				relative, stripMiddlewareName,
			)
			continue
		}

		stripped := make([]string, 0, len(definition.Headers.CustomRequestHeaders))
		for name, value := range definition.Headers.CustomRequestHeaders {
			if value != "" {
				t.Errorf(
					"%s: %q sets %s to %q. Only an EMPTY value deletes a "+
						"header; a non-empty one makes the edge assert that "+
						"identity for every caller.",
					relative, stripMiddlewareName, name, value,
				)
				continue
			}
			stripped = append(stripped, name)
		}
		sort.Strings(stripped)
		strippedPerFile[relative] = stripped

		present := map[string]bool{}
		for _, name := range stripped {
			present[name] = true
		}
		var missing []string
		for _, name := range requiredStrippedHeaders {
			if !present[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			t.Errorf(
				"%s: %q does not delete %s.\n"+
					"A middleware that still resolves and still strips most "+
					"names reads as correct in review and in "+
					"TestEveryRouterMiddlewareResolves. Every name left in "+
					"place is one a client can choose for itself.",
				relative, stripMiddlewareName, strings.Join(missing, ", "),
			)
		}

		// 2. Every router that reaches elitea-main must NAME the middleware. A
		//    definition nothing references strips nothing.
		routersReachingMain := 0
		for name, router := range parsed.HTTP.Routers {
			if router.Service != protectedService {
				continue
			}
			routersReachingMain++
			carries := false
			for _, named := range router.Middlewares {
				if providerLocalName(named) == stripMiddlewareName {
					carries = true
					break
				}
			}
			if !carries {
				t.Errorf(
					"%s: router %q reaches service %q without %q.\n"+
						"Defining the middleware is not enough — a router that "+
						"does not name it forwards the client's own "+
						"X-Auth-ID, and elitea-main accepts it (#326).\n"+
						"Add it to that router's middlewares list.",
					relative, name, protectedService, stripMiddlewareName,
				)
			}
		}
		if routersReachingMain == 0 {
			t.Errorf(
				"%s declares no router with service %q, so this gate checked "+
					"nothing in it. Either the service was renamed or the file "+
					"changed shape; update protectedService in this file.",
				relative, protectedService,
			)
		}
	}

	// 3. The two files must strip the SAME names. They are copies on purpose:
	//    dynamic.e2e.yml REPLACES dynamic.yml at runtime, so each stack loads
	//    exactly one of them. Adding a name to one alone leaves the other stack
	//    open, which is the #326 defect repeated.
	if len(strippedPerFile) == len(browserEdgeFiles) {
		first := browserEdgeFiles[0]
		for _, other := range browserEdgeFiles[1:] {
			if strings.Join(strippedPerFile[first], ",") == strings.Join(strippedPerFile[other], ",") {
				continue
			}
			t.Errorf(
				"%s and %s strip different header sets:\n  %s: %s\n  %s: %s\n"+
					"One file replaces the other at runtime, so each stack "+
					"loads only one of them. A name added to one alone leaves "+
					"the other stack open (#326).",
				first, other,
				first, strings.Join(strippedPerFile[first], ", "),
				other, strings.Join(strippedPerFile[other], ", "),
			)
		}
	}
}
