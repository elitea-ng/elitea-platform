package toolkitcatalogue

// What the admitted worker can actually BUILD, published as a verdict the
// admin page renders beside each type.
//
// # The problem this answers
//
// The served catalogue and the runtime that executes it disagree, and nothing
// on any screen said so: the Rust worker materialises its native families and
// SKIPS every other type with a warning (`agent_toolkit_skipped`). It does not
// refuse. So a toolkit of an unported type attaches, and then does nothing.
//
// An operator deciding whether to offer a tile needs that fact in front of
// them. This file is the fact.
//
// # UNVERIFIED IS NOT UNUSABLE, AND THIS NEVER HIDES A TYPE
//
// The verdict is ADVISORY. It changes nothing about what the catalogue serves:
// `Filter` reads the policy tables and this file is not consulted there. The
// catalogue's own worker-capability projection (runtimecomposition) is what
// hides a type the worker cannot run.
//
// # Why the list is Go source and not a generated file
//
// It is a few dozen short strings that change when the worker changes. A
// generator would add a build step, a checked-in artefact and a way for the
// artefact to go stale unnoticed. `capability_test.go` re-reads the upstream
// source on every run and fails naming the file that moved, which is the same
// guarantee with nothing to regenerate.

import (
	"sort"
	"strings"
)

// Verdict is the resolved worker support for one toolkit type.
type Verdict string

const (
	// VerdictSupported means the Rust worker has a native family for the type.
	VerdictSupported Verdict = "supported"
	// VerdictUnverified means the worker is not known to carry it. It is NOT a
	// claim that the type fails — see this file's header.
	VerdictUnverified Verdict = "unverified"
)

// Capability is one type's verdict, with the sentence the page shows.
type Capability struct {
	ToolkitType string
	Verdict     Verdict
	// Rust reports whether services/elitea-worker-rust materialises a native
	// family for this type.
	Rust bool
	// Reason is a complete sentence naming WHICH runtime fact produced the
	// verdict. A bare boolean on the page would send the operator to read Go
	// source to find out what it measured.
	Reason string
}

// rustNativeToolTypes are the tool types services/elitea-worker-rust
// materialises natively, spelled as that worker spells them.
//
// Transcribed from `src/toolkits/materialize.rs`: the match arms of
// `materialize_a_to_k`, `materialize_p_to_z`, `materialize_ado`,
// `materialize_zephyr_rest` and `materialize_test_management`, plus the three
// types dispatched by direct comparison (`artifact`, `openapi`, `sharepoint`).
//
// `pptx`, `docx` and `xlsx` (ADR-0032 P3, P4) are claim-scoped families like
// `artifact`: they materialise only where the Office host is lent (the ordinary
// agent path and a pipeline's nodes of a worker built with the `toolkit-office`
// feature, which the cloud worker enables and the desktop host does not).
// `pptx` serves the SDK's two tools plus nineteen of its own, and `docx` and
// `xlsx` have no SDK counterpart and serve all of theirs, so no per-tool list
// is needed here.
//
// `imagegen` (#864) is a claim-scoped family like `pptx`: it materialises only
// where the Office host that reaches the gateway's images API is lent (the
// ordinary agent path, a pipeline's nodes and a direct toolkit call of the
// cloud worker), and it serves the SDK's two tools, `generate_image` and
// `edit_image`, unchanged. Both write artifacts, so a direct
// `toolkit.call_tool.v1` (read-only tools only) lists them and refuses to run
// them with that reason.
//
// `memory` (worker unit M9) is a claim-scoped family like the index tools: it
// serves the SDK's four tools unchanged, over elitea-vector, and materialises
// only where the claim carries a vector token (the Rust indexing runtime) and
// the deployment names a memory embedding model. runtimecomposition drops it
// from the Rust worker's catalogue projection when the deployment does not
// index with the Rust runtime.
//
// `artifact` (#906) is the odd one: it materialises only where the live
// execution claim is in scope (the ordinary agent path and a root pipeline's
// direct and LLM nodes), because its authority is that claim rather than a
// credential in the frozen snapshot. A saved child pipeline or nested agent
// still skips it: a direct node there is refused as unsupported at assembly,
// and an LLM node there finds the tools unavailable when it runs.
//
// Several of these families are partial (no indexing tools, and some tools
// that need a model or artifact bytes the runtime does not lend). The
// per-tool split lives in the capability snapshot's `supported_tools`; this
// list only says the type is dispatched. It is checked against the dispatch,
// not the directory listing, so a family on disk that no arm reaches would
// fail TestRustNativeToolTypesMatchTheMaterialiser rather than claim support.
var rustNativeToolTypes = []string{
	"ado_boards",
	"ado_plans",
	"ado_repos",
	"ado_wiki",
	"aha",
	"artifact",
	"azure",
	"azure_search",
	"bigquery",
	"bitbucket",
	"carrier",
	"confluence",
	"docx",
	"elastic",
	"figma",
	"gcp",
	"github",
	"gitlab",
	"gitlab_org",
	"google_places",
	"host_intel",
	"imagegen",
	"jira",
	"k8s",
	"keycloak",
	"memory",
	"openapi",
	"postman",
	"pptx",
	"qtest",
	"rally",
	"report_portal",
	"salesforce",
	"service_now",
	"sharepoint",
	"slack",
	"sonar",
	"sql",
	"testio",
	"testrail",
	"web",
	"xlsx",
	"xray_cloud",
	"yagmail",
	"zephyr",
	"zephyr_enterprise",
	"zephyr_essential",
	"zephyr_scale",
	"zephyr_squad",
}

// importKeyToToolkitType maps the SDK/worker spelling to the CATALOGUE key,
// which is the Pydantic schema title.
//
// One entry, and it is the trap the survey names: the schema title is
// `kubernetes` while the SDK registry key and the Rust match arm are both
// `k8s`. A capability list keyed on the import key silently never matches the
// catalogue, so the page would report every type unverified and nobody would
// know why.
var importKeyToToolkitType = map[string]string{
	"k8s": "kubernetes",
	// The SDK's shared Azure DevOps base. It is an import key with no schema of
	// its own, so it names no catalogue type. Mapped to the empty string rather
	// than left out, so that a reader of this map sees it was considered.
	"ado": "",
}

// PinnedWorkerCapabilities answers from the list above.
//
// A value, not a pointer, and it holds no state: the verdict for a type is a
// pure function of one constant set, so there is nothing to construct, nothing
// to inject and no nil receiver to guard.
type PinnedWorkerCapabilities struct{}

// ToolkitCapability resolves one catalogue type.
func (PinnedWorkerCapabilities) ToolkitCapability(toolkitType string) Capability {
	key := strings.TrimSpace(toolkitType)
	return capabilityVerdict(key, rustNativeTypes()[key])
}

// capabilityVerdict is the verdict table for one type given whether the Rust
// worker carries it.
func capabilityVerdict(key string, rust bool) Capability {
	capability := Capability{ToolkitType: key, Rust: rust}
	if rust {
		capability.Verdict = VerdictSupported
		capability.Reason = "the Rust worker carries a native family for this type"
		return capability
	}
	capability.Verdict = VerdictUnverified
	capability.Reason = "the Rust worker is not known to carry this type: it has no native " +
		"family for it. The type may still work; it is not verified"
	return capability
}

// rustNativeTypes projects the list onto catalogue keys. Built on each call
// rather than in a package-level map so that no caller can mutate a shared set
// — the same rule `toolkitTypeSchemas` had to learn.
func rustNativeTypes() map[string]bool { return catalogueKeys(rustNativeToolTypes) }

func catalogueKeys(importKeys []string) map[string]bool {
	keys := make(map[string]bool, len(importKeys))
	for _, importKey := range importKeys {
		mapped, remapped := importKeyToToolkitType[importKey]
		if remapped {
			if mapped == "" {
				continue
			}
			keys[mapped] = true
			continue
		}
		keys[importKey] = true
	}
	return keys
}

// CapabilityTypes lists every catalogue type the worker is known to carry,
// sorted. Tests and the drift check read it; nothing on the request path does.
func CapabilityTypes() []string {
	seen := map[string]bool{}
	for key := range rustNativeTypes() {
		seen[key] = true
	}
	types := make([]string, 0, len(seen))
	for key := range seen {
		types = append(types, key)
	}
	sort.Strings(types)
	return types
}
