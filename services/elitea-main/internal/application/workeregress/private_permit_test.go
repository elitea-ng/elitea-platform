package workeregress

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func permitResolver(t *testing.T, mode platformconfig.WorkerEgressMode, permit []string, inputs *inputStub) (*Resolver, *projectStub) {
	t.Helper()
	projects := &projectStub{entries: map[int64][]string{7: {"10.9.0.0/16"}}}
	resolver, err := NewResolver(platformStub{settings: platformconfig.WorkerEgressSettings{Mode: mode, PrivatePermit: permit}},
		projects, inputs, &governedStub{})
	if err != nil {
		t.Fatal(err)
	}
	return resolver, projects
}

// #2950: an open platform with a private permit sends an open-mode policy
// that carries only the permit, normalised and sorted, and still reads
// nothing beyond the platform setting.
func TestOpenWithAPrivatePermitSendsOnlyThePermit(t *testing.T) {
	inputs := &inputStub{}
	resolver, projects := permitResolver(t, platformconfig.WorkerEgressOpen,
		[]string{"Jira.Corp.Example", "10.20.0.0/16", "jira.corp.example"}, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
	if err != nil {
		t.Fatal(err)
	}
	if policy.GetMode() != runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_OPEN || len(policy.GetAllow()) != 0 {
		t.Fatalf("policy %v", policy)
	}
	if want := []string{"10.20.0.0/16", "jira.corp.example"}; !slices.Equal(policy.GetPrivateAllow(), want) {
		t.Fatalf("private_allow = %v, want %v", policy.GetPrivateAllow(), want)
	}
	if projects.calls != 0 || inputs.calls != 0 {
		t.Fatalf("open read the project (%d) or the input (%d)", projects.calls, inputs.calls)
	}
}

// In allowlist mode the permit rides beside the allowlist, and only the
// platform setting supplies it: neither the project's entries nor a toolkit's
// URL (an on-premises Jira in agentInput) is ever added to it.
func TestThePrivatePermitComesFromThePlatformOnly(t *testing.T) {
	inputs := &inputStub{entries: map[string][]byte{"bundle-1/agent-request": agentInput(t)}}
	resolver, _ := permitResolver(t, platformconfig.WorkerEgressAllowlist, []string{"*.corp.example:8443"}, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(policy.GetAllow(), "jira.corp.example") || !slices.Contains(policy.GetAllow(), "10.9.0.0/16") {
		t.Fatalf("allow = %v", policy.GetAllow())
	}
	if want := []string{"*.corp.example:8443"}; !slices.Equal(policy.GetPrivateAllow(), want) {
		t.Fatalf("private_allow = %v, want %v", policy.GetPrivateAllow(), want)
	}
	// No permit: the field stays empty in allowlist mode too.
	resolver, _ = permitResolver(t, platformconfig.WorkerEgressAllowlist, nil, inputs)
	policy, err = resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
	if err != nil || len(policy.GetPrivateAllow()) != 0 {
		t.Fatalf("no permit: %v %v", policy, err)
	}
}

// A malformed or oversized permit refuses the run in either mode, naming the
// entry or the bound; it is never truncated or skipped.
func TestAMalformedPrivatePermitRefuses(t *testing.T) {
	for _, mode := range []platformconfig.WorkerEgressMode{platformconfig.WorkerEgressOpen, platformconfig.WorkerEgressAllowlist} {
		resolver, _ := permitResolver(t, mode, []string{"ok.example", "https://jira.corp.example"}, &inputStub{})
		_, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
		refused, ok := AsRefusal(err)
		if !ok || !strings.Contains(refused.Message, "private permit") || !strings.Contains(refused.Message, `"https://jira.corp.example"`) {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	many := make([]string, 0, MaxPrivateEntries+1)
	for i := range MaxPrivateEntries + 1 {
		many = append(many, fmt.Sprintf("h%d.corp.example", i))
	}
	if _, err := AssemblePrivatePermit(many); err == nil {
		t.Fatal("257 entries were assembled")
	}
	if permit, err := AssemblePrivatePermit(many[:MaxPrivateEntries]); err != nil || len(permit) != MaxPrivateEntries {
		t.Fatalf("256 entries: %d %v", len(permit), err)
	}
}

// The deployment's permit (ELITEA_WORKER_EGRESS_PRIVATE_PERMIT) is parsed
// fail-closed and merged with the section's, deduplicated and sorted.
func TestTheOperatorPermitMergesWithTheSection(t *testing.T) {
	entries, err := ParseOperatorPermit(" ado-mock:8097, 10.20.0.0/16 ,,")
	if err != nil || !slices.Equal(entries, []string{"ado-mock:8097", "10.20.0.0/16"}) {
		t.Fatalf("parse = %v %v", entries, err)
	}
	if _, err := ParseOperatorPermit("ok.example,https://x.example/"); err == nil || !strings.Contains(err.Error(), OperatorPermitEnv) {
		t.Fatalf("malformed: %v", err)
	}
	tooMany := strings.Repeat("a.example,", platformconfig.MaxWorkerEgressPrivateEntries+1)
	if _, err := ParseOperatorPermit(tooMany); err == nil || !strings.Contains(err.Error(), "at most 128") {
		t.Fatalf("too many: %v", err)
	}
	if entries, err := ParseOperatorPermit(""); err != nil || len(entries) != 0 {
		t.Fatalf("empty: %v %v", entries, err)
	}
	resolver, _ := permitResolver(t, platformconfig.WorkerEgressOpen, []string{"10.20.0.0/16"}, &inputStub{})
	resolver.WithOperatorPermit([]string{"ado-mock:8097", "10.20.0.0/16"})
	policy, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.20.0.0/16", "ado-mock:8097"}; !slices.Equal(policy.GetPrivateAllow(), want) {
		t.Fatalf("private_allow = %v, want %v", policy.GetPrivateAllow(), want)
	}
}

// A schema-only configuration validation opens no connection, so a permit
// that cannot be assembled never refuses it, in either mode.
func TestAValidationIsNeverRefusedForThePermit(t *testing.T) {
	validation := &runtimev1.WorkerCommandV1{
		CapabilityCommand: &runtimev1.WorkerCommandV1_ConfigurationValidation{
			ConfigurationValidation: &runtimev1.ConfigurationValidationCommandV1{SettingsEntryId: "settings"},
		},
	}
	open, _ := permitResolver(t, platformconfig.WorkerEgressOpen, []string{"https://not-an-entry"}, &inputStub{})
	if policy, err := open.WorkerEgressPolicy(context.Background(), validation); err != nil || policy != nil {
		t.Fatalf("open: %v %v", policy, err)
	}
	allowlist, _ := permitResolver(t, platformconfig.WorkerEgressAllowlist, []string{"https://not-an-entry"}, &inputStub{})
	policy, err := allowlist.WorkerEgressPolicy(context.Background(), validation)
	if err != nil || len(policy.GetAllow()) != 0 || len(policy.GetPrivateAllow()) != 0 {
		t.Fatalf("allowlist: %v %v", policy, err)
	}
}
