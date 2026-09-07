package toolkitcatalogue

// What the admitted workers can actually BUILD, published as a verdict the
// admin page renders beside each type.
//
// # The problem this answers
//
// The served catalogue and the runtimes that execute it disagree, and nothing
// on any screen said so:
//
//   - The Python worker image installs the SDK extras `[agent-current,
//     indexing-current]`, a curated subset of `elitea-sdk[all]`.
//     `services/elitea-worker-python/elitea-sdk.lock.json` build-verifies
//     exactly eighteen SDK tool import keys. A type outside that set whose
//     third-party dependency is absent lands in the SDK's FAILED_IMPORTS and
//     fails at AGENT-RUN time, not at toolkit-create time.
//   - The Rust worker materialises twenty-two native families and SKIPS every
//     other type with a warning (`agent_toolkit_skipped`). It does not refuse.
//     So a `STANDALONE_WORKER=rust` deployment silently drops most of the
//     catalogue: the toolkit attaches, and then does nothing.
//
// An operator deciding whether to offer a tile needs that fact in front of
// them. This file is the fact.
//
// # UNVERIFIED IS NOT UNUSABLE, AND THIS NEVER HIDES A TYPE
//
// The verdict is ADVISORY. It changes nothing about what the catalogue serves:
// `Filter` reads the policy tables and this file is not consulted there. That is
// deliberate and it is the honest reading of what these two lists are.
//
// The Python list is what the image build VERIFIES, not what it can import. An
// extra that happens to be present transitively still works; the lock simply
// does not assert it. Turning "not build-verified" into "withheld" would take
// roughly thirty working types away from every deployment on the strength of a
// list that was never a capability manifest.
//
// The real fix is a worker that publishes its own FAILED_IMPORTS at admission,
// which is a separate change to `services/elitea-worker-python` and to the
// admission plane. When that lands, `Source` gains a `worker_capability` member
// and this file becomes its static fallback. Until then the page states what is
// known and the operator decides.
//
// # Why the lists are Go source and not a generated file
//
// They are two dozen short strings that change when a worker changes. A
// generator would add a build step, a checked-in artefact and a way for the
// artefact to go stale unnoticed. `capability_drift_test.go` re-reads BOTH
// upstream sources on every run and fails naming the file that moved, which is
// the same guarantee with nothing to regenerate.

import (
	"sort"
	"strings"
)

// Verdict is the resolved worker support for one toolkit type.
type Verdict string

const (
	// VerdictSupported means at least one admitted worker is known to carry the
	// type: the Python image build-verifies its SDK import, or the Rust worker
	// has a native family for it.
	VerdictSupported Verdict = "supported"
	// VerdictUnverified means neither is known to carry it. It is NOT a claim
	// that the type fails — see this file's header.
	VerdictUnverified Verdict = "unverified"
)

// Capability is one type's verdict, with the sentence the page shows.
type Capability struct {
	ToolkitType string
	Verdict     Verdict
	// Python reports whether elitea-sdk.lock.json's indexing capability profile
	// build-verifies this type's SDK tool import in the admitted image.
	Python bool
	// Rust reports whether services/elitea-worker-rust materialises a native
	// family for this type.
	Rust bool
	// Reason is a complete sentence naming WHICH runtime fact produced the
	// verdict. A bare boolean on the page would send the operator to read Go
	// source to find out what it measured.
	Reason string
}

// pythonVerifiedImportKeys are the SDK tool import keys the admitted Python
// worker image build-verifies.
//
// Transcribed from `indexing_capability_profile.required_sdk_tool_import_keys`
// in services/elitea-worker-python/elitea-sdk.lock.json. `capability_drift_test.go`
// re-reads that file and fails if this list stops matching it.
//
// THESE ARE IMPORT KEYS, NOT CATALOGUE TYPES. `toolkitTypeForImportKey` maps
// the one that differs. `ado` is the SDK's shared Azure DevOps base family and
// is not a catalogue type at all; it maps to nothing and is dropped.
var pythonVerifiedImportKeys = []string{
	"ado",
	"ado_boards",
	"ado_plans",
	"ado_repos",
	"ado_wiki",
	"bitbucket",
	"confluence",
	"figma",
	"github",
	"gitlab",
	"jira",
	"qtest",
	"sharepoint",
	"testrail",
	"xray_cloud",
	"zephyr_enterprise",
	"zephyr_essential",
	"zephyr_scale",
}

// rustNativeToolTypes are the tool types services/elitea-worker-rust
// materialises natively, spelled as that worker spells them.
//
// Transcribed from `src/toolkits/materialize.rs`: the match arms of
// `materialize_a_to_k` and `materialize_p_to_z`, plus the two types dispatched
// by direct comparison (`openapi`, `sharepoint`).
//
// `aha` is NOT here although the family is complete in that tree: no dispatch
// reaches it, so the worker skips an `aha` toolkit like any unsupported type.
// A list built from the directory listing would have claimed support the
// runtime does not give — which is the same "wired nowhere" defect this
// repository keeps finding.
var rustNativeToolTypes = []string{
	"azure",
	"azure_search",
	"elastic",
	"gcp",
	"github",
	"gitlab_org",
	"google_places",
	"k8s",
	"keycloak",
	"openapi",
	"postman",
	"rally",
	"report_portal",
	"salesforce",
	"service_now",
	"sharepoint",
	"slack",
	"sonar",
	"sql",
	"yagmail",
	"zephyr",
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

// PinnedWorkerCapabilities answers from the two lists above.
//
// A value, not a pointer, and it holds no state: the verdict for a type is a
// pure function of two constant sets, so there is nothing to construct, nothing
// to inject and no nil receiver to guard.
type PinnedWorkerCapabilities struct{}

// ToolkitCapability resolves one catalogue type.
func (PinnedWorkerCapabilities) ToolkitCapability(toolkitType string) Capability {
	key := strings.TrimSpace(toolkitType)
	python := pythonVerifiedTypes()[key]
	rust := rustNativeTypes()[key]

	capability := Capability{ToolkitType: key, Python: python, Rust: rust}
	switch {
	case python && rust:
		capability.Verdict = VerdictSupported
		capability.Reason = "the admitted Python worker image build-verifies this type's SDK import, " +
			"and the Rust worker carries a native family for it"
	case python:
		capability.Verdict = VerdictSupported
		capability.Reason = "the admitted Python worker image build-verifies this type's SDK import"
	case rust:
		capability.Verdict = VerdictSupported
		capability.Reason = "the Rust worker carries a native family for this type; " +
			"the Python worker image does not build-verify its SDK import"
	default:
		capability.Verdict = VerdictUnverified
		capability.Reason = "no admitted worker is known to carry this type: the Python image's " +
			"pinned extras do not build-verify its SDK import, and the Rust worker has no native " +
			"family for it. The type may still work; it is not verified"
	}
	return capability
}

// pythonVerifiedTypes and rustNativeTypes project the two lists onto catalogue
// keys. Built on each call rather than in a package-level map so that no caller
// can mutate a shared set — the same rule `toolkitTypeSchemas` had to learn.
func pythonVerifiedTypes() map[string]bool { return catalogueKeys(pythonVerifiedImportKeys) }

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

// CapabilityTypes lists every catalogue type either worker is known to carry,
// sorted. Tests and the drift check read it; nothing on the request path does.
func CapabilityTypes() []string {
	seen := map[string]bool{}
	for key := range pythonVerifiedTypes() {
		seen[key] = true
	}
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
