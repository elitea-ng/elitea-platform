package execution

import (
	"testing"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestJobStateIncludesDurableQuarantine(t *testing.T) {
	states := []JobState{
		JobPending,
		JobDispatched,
		JobClaimed,
		JobRunning,
		JobSettling,
		JobSucceeded,
		JobFailed,
		JobCancelled,
		JobQuarantined,
	}
	for _, state := range states {
		if !state.Valid() {
			t.Fatalf("durable job state %q is not valid", state)
		}
	}
	if JobState("UNKNOWN").Valid() {
		t.Fatal("unknown durable job state was accepted")
	}
}

func TestSupportedCapabilityIncludesLanguageNeutralAgentSemantics(t *testing.T) {
	for _, capabilityID := range []string{
		ConfigurationValidationCapability,
		IndexIngestCapability,
		AgentApplicationCapability,
		AgentAdhocCapability,
	} {
		if !SupportedCapability(capabilityID) {
			t.Fatalf("capability %q is not supported", capabilityID)
		}
		job := Job{
			ID:                  "execution",
			CommandID:           "command",
			TenantID:            "tenant",
			ResourceProjectID:   "1",
			ProjectionProjectID: "1",
			ActorID:             "7",
			CapabilityID:        capabilityID,
			Generation:          1,
			State:               JobPending,
			CreatedAt:           time.Now(),
		}
		if err := job.Validate(); err != nil {
			t.Fatalf("Job.Validate(%q) error = %v", capabilityID, err)
		}
	}
	if SupportedCapability("index.search.v1") {
		t.Fatal("standalone index search was accepted as an execution capability")
	}
}

func TestNodeEventCapabilitiesExcludeControlOnlyValidation(t *testing.T) {
	for _, capabilityID := range []string{
		IndexIngestCapability,
		AgentApplicationCapability,
		AgentAdhocCapability,
	} {
		if !NodeEventCapability(capabilityID) {
			t.Fatalf("capability %q cannot publish NodeEvents", capabilityID)
		}
	}
	if NodeEventCapability(ConfigurationValidationCapability) {
		t.Fatal("configuration validation was allowed onto the NodeEvent data plane")
	}
}

func TestAgentExecutionBindingRequiresOneImmutableProtobufEntry(t *testing.T) {
	content := []byte("agent-input")
	bundle := InputBundle{
		ID:        "bundle",
		Version:   "version",
		MediaType: InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256([]byte("manifest")),
		Manifest:  []byte("manifest"),
		Entries: []InputEntry{{
			ID:                    "agent-request",
			Version:               "entry-version",
			SemanticRole:          AgentExecutionRequestRole,
			ContentID:             "content",
			MediaType:             AgentExecutionInputMediaType,
			Classification:        "tenant-confidential",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
			ContentDigest:         runtimedomain.SHA256(content),
			ContentLength:         int64(len(content)),
			Content:               content,
		}},
	}
	binding := AgentExecutionBinding{
		RequestEntryID:            "agent-request",
		ClientStreamID:            "conversation-1",
		ClientMessageID:           "message-1",
		ClientExecutionGeneration: "generation-1",
		SIOEvent:                  "chat_predict",
	}
	if err := binding.Validate(bundle); err != nil {
		t.Fatalf("binding.Validate() error = %v", err)
	}
	continued := binding
	continued.SIOEvent = "chat_continue_predict"
	if err := continued.Validate(bundle); err != nil {
		t.Fatalf("continued binding.Validate() error = %v", err)
	}

	invalid := binding
	invalid.SIOEvent = "test_toolkit_tool"
	if err := invalid.Validate(bundle); err == nil {
		t.Fatal("agent binding accepted a toolkit event")
	}
	invalid = binding
	invalid.ClientStreamID = "conversation\nforged"
	if err := invalid.Validate(bundle); err == nil {
		t.Fatal("agent binding accepted an unsafe stream identity")
	}
	invalid = binding
	invalid.ClientExecutionGeneration = ""
	if err := invalid.Validate(bundle); err == nil {
		t.Fatal("agent binding accepted an empty client execution generation")
	}
}

func TestInputBundleUsesMediaSpecificContentBounds(t *testing.T) {
	if maxInputEntryContentBytes(SettingsJSONMediaType) != MaxInputEntryContentBytes {
		t.Fatal("configuration input bound changed")
	}
	if maxInputEntryContentBytes(AgentExecutionInputMediaType) != MaxAgentExecutionInputBytes {
		t.Fatal("agent input bound does not match the public worker contract")
	}
	if maxInputEntryContentBytes("application/octet-stream") != 0 {
		t.Fatal("unknown input media type was accepted")
	}
}

// The settings entry and the arguments entry carry different trust. The
// settings are redeemed by this service from the saved toolkit row and hold
// credentials; the arguments come from the caller. This binding is where that
// distinction becomes enforceable, so every way of blurring it must fail.
func TestToolkitCallToolBindingRequiresTwoDistinctlyRoledEntries(t *testing.T) {
	settings := []byte(`{"url":"https://github.example"}`)
	arguments := []byte(`{"issue":7}`)
	entry := func(id, role string, content []byte) InputEntry {
		return InputEntry{
			ID:                    id,
			Version:               "entry-version",
			SemanticRole:          role,
			ContentID:             "content-" + id,
			MediaType:             SettingsJSONMediaType,
			Classification:        "tenant-confidential",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
			ContentDigest:         runtimedomain.SHA256(content),
			ContentLength:         int64(len(content)),
			Content:               content,
		}
	}
	bundle := InputBundle{
		ID:        "bundle",
		Version:   "version",
		MediaType: InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256([]byte("manifest")),
		Manifest:  []byte("manifest"),
		Entries: []InputEntry{
			entry("toolkit-settings", ToolkitCallToolSettingsRole, settings),
			entry("tool-arguments", ToolkitCallToolArgumentsRole, arguments),
		},
	}
	binding := ToolkitCallToolBinding{
		ToolkitType:      "github",
		ToolName:         "get_issue",
		ToolkitID:        17,
		ToolkitVersion:   "3",
		SettingsEntryID:  "toolkit-settings",
		ArgumentsEntryID: "tool-arguments",
	}
	if err := binding.Validate(bundle); err != nil {
		t.Fatalf("binding.Validate() error = %v", err)
	}
	// An absent toolkit version is normal: not every deployment records one.
	optional := binding
	optional.ToolkitVersion = ""
	if err := optional.Validate(bundle); err != nil {
		t.Fatalf("binding.Validate() refused an absent toolkit version: %v", err)
	}

	for name, mutate := range map[string]func(*ToolkitCallToolBinding){
		"one entry serving as both": func(b *ToolkitCallToolBinding) {
			b.ArgumentsEntryID = b.SettingsEntryID
		},
		"no tool named":     func(b *ToolkitCallToolBinding) { b.ToolName = "" },
		"no toolkit type":   func(b *ToolkitCallToolBinding) { b.ToolkitType = "" },
		"no toolkit row":    func(b *ToolkitCallToolBinding) { b.ToolkitID = 0 },
		"forged tool name":  func(b *ToolkitCallToolBinding) { b.ToolName = "get\nissue" },
		"unbound arguments": func(b *ToolkitCallToolBinding) { b.ArgumentsEntryID = "elsewhere" },
	} {
		invalid := binding
		mutate(&invalid)
		if err := invalid.Validate(bundle); err == nil {
			t.Fatalf("tool-run binding accepted %s", name)
		}
	}

	// The roles must match the entries, not merely the ids: a bundle that put
	// caller arguments under the settings role would otherwise pass.
	swapped := bundle
	swapped.Entries = []InputEntry{
		entry("toolkit-settings", ToolkitCallToolArgumentsRole, settings),
		entry("tool-arguments", ToolkitCallToolSettingsRole, arguments),
	}
	if err := binding.Validate(swapped); err == nil {
		t.Fatal("tool-run binding accepted swapped semantic roles")
	}

	// A third entry nothing binds is an input the worker would fetch without
	// the command ever naming it.
	extra := bundle
	extra.Entries = append(append([]InputEntry{}, bundle.Entries...),
		entry("stowaway", ToolkitCallToolArgumentsRole, arguments))
	if err := binding.Validate(extra); err == nil {
		t.Fatal("tool-run binding accepted an unbound extra entry")
	}
}

func TestToolkitCallToolIsASupportedCapability(t *testing.T) {
	if !SupportedCapability(ToolkitCallToolCapability) {
		t.Fatal("the kernel refuses the tool-run capability it now admits")
	}
	// It emits no NodeEvents: one call, one answer. A progress projection with
	// no reader is worse than none.
	if NodeEventCapability(ToolkitCallToolCapability) {
		t.Fatal("the tool-run capability claims a progress projection it never writes")
	}
}
