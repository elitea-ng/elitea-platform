package codesandbox

import (
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"os"
	"strings"
	"testing"
)

func platformClaimsFixture() PlatformGrantClaims {
	execution := "0123456789abcdef0123456789abcdef"
	activation := strings.Repeat("1", 64)
	dispatch := strings.Repeat("2", 64)
	fingerprint := strings.Repeat("4", 64)
	return PlatformGrantClaims{Schema: "elitea.sandbox.code-platform-owner-grant.v1", Purpose: "platform_broker_runtime", TenantID: "7", ProjectID: 7, ExecutionID: execution, OriginalGeneration: 1, ClaimID: "23456789abcdef0123456789abcdef01", ClaimAttempt: 2, LeaseEpoch: 3, FenceSHA256: strings.Repeat("3", 64), ActivationID: activation, Attempt: 1, DispatchActivation: dispatch, JobKey: JobKey(execution, dispatch), RequestDigest: fingerprint, BindingSHA256: strings.Repeat("5", 64), PreparedSHA256: strings.Repeat("6", 64), PreparedFingerprint: fingerprint, PolicySHA256: strings.Repeat("7", 64), MaxCalls: 16, MaxTotalBytes: 1048576, SupervisorAudience: "spiffe://elitea/supervisor/one", RequesterWorkloadIdentity: "spiffe://elitea/main", Operation: "read_retained_runtime", IssuedAtMillis: 1000, ExpiresAtMillis: 21000}
}
func TestCodePlatformGrantSeparatesFingerprintsPurposeAndClosedOperations(t *testing.T) {
	valid := platformClaimsFixture()
	if valid.Validate() != nil {
		t.Fatal("valid plain grant refused")
	}
	for _, tc := range []struct {
		name   string
		change func(*PlatformGrantClaims)
	}{
		{"compiled request", func(c *PlatformGrantClaims) { c.RequestDigest = strings.Repeat("8", 64) }},
		{"Compile purpose cannot call platform", func(c *PlatformGrantClaims) { c.Purpose = "compile" }},
		{"raw SHA substituted for fingerprint", func(c *PlatformGrantClaims) { c.PreparedFingerprint = c.PreparedSHA256 }},
		{"recovery purpose", func(c *PlatformGrantClaims) { c.Purpose = "whole_code_execute" }},
		{"submit operation", func(c *PlatformGrantClaims) { c.Operation = "submit" }},
		{"read with reply selectors", func(c *PlatformGrantClaims) { n := uint64(1); c.Sequence = &n }},
		{"publish missing record", func(c *PlatformGrantClaims) { c.Operation = "publish_committed_platform_reply" }},
		{"other job", func(c *PlatformGrantClaims) { c.JobKey = strings.Repeat("9", 64) }},
		{"overlong lease", func(c *PlatformGrantClaims) { c.ExpiresAtMillis = c.IssuedAtMillis + 30001 }},
		{"unbounded call count", func(c *PlatformGrantClaims) { c.MaxCalls = 4097 }},
		{"unbounded bytes", func(c *PlatformGrantClaims) { c.MaxTotalBytes = 67108865 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := valid
			tc.change(&changed)
			if changed.Validate() == nil {
				t.Fatal("invalid platform authority admitted")
			}
		})
	}
	publish := valid
	publish.Operation = "publish_committed_platform_reply"
	sequence := uint64(16)
	request := strings.Repeat("a", 64)
	reply := strings.Repeat("b", 64)
	publish.Sequence = &sequence
	publish.PlatformRequestSHA256 = &request
	publish.CommittedReplySHA256 = &reply
	if publish.Validate() != nil {
		t.Fatal("exact committed call refused")
	}
	sequence = 17
	if publish.Validate() == nil {
		t.Fatal("sequence exceeded original policy")
	}
}
func TestCodePlatformRuntimeMustMatchOriginalFinalBinding(t *testing.T) {
	c := platformClaimsFixture()
	r := RetainedRuntime{Kind: "docker", RuntimeID: strings.Repeat("c", 64), OwnerEpoch: 3, BindingSHA256: c.BindingSHA256, PreparedSHA256: c.PreparedSHA256, PreparedFingerprint: c.PreparedFingerprint, PolicySHA256: c.PolicySHA256, MaxCalls: c.MaxCalls, MaxTotalBytes: c.MaxTotalBytes, Lifecycle: "dispatched"}
	if !r.Matches(c) {
		t.Fatal("exact owner runtime refused")
	}
	for _, tc := range []struct {
		name   string
		change func(*RetainedRuntime)
	}{{"changed runtime epoch", func(r *RetainedRuntime) { r.OwnerEpoch = 0 }}, {"raw/fingerprint substitution", func(r *RetainedRuntime) { r.PreparedFingerprint = r.PreparedSHA256 }}, {"changed policy", func(r *RetainedRuntime) { r.PolicySHA256 = strings.Repeat("d", 64) }}, {"changed original binding", func(r *RetainedRuntime) { r.BindingSHA256 = strings.Repeat("e", 64) }}, {"undispatched runtime", func(r *RetainedRuntime) { r.Lifecycle = "reserved" }}, {"foreign backend", func(r *RetainedRuntime) { r.Kind = "process" }}} {
		t.Run(tc.name, func(t *testing.T) {
			changed := r
			tc.change(&changed)
			if changed.Matches(c) {
				t.Fatal("foreign/uncommitted owner runtime admitted")
			}
		})
	}
}

func platformCompiledFixture(t *testing.T) (PlatformGrantClaims, PlatformCompiledExecute) {
	t.Helper()
	c := platformClaimsFixture()
	raw, err := os.ReadFile("../runtime/testdata/compiled-snapshot-v1/binding.json")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runtime.ParseRustSnapshotBinding(raw)
	if err != nil {
		t.Fatal(err)
	}
	binding.TenantID = c.TenantID
	binding.ProjectID = int32(c.ProjectID)
	binding.PolicyRevision = "cargo-broker-execute-v1"
	binding.BasePreparedRequestSHA256 = c.PreparedFingerprint
	key, err := binding.Key()
	if err != nil {
		t.Fatal(err)
	}
	selector := PlatformCompiledExecute{Binding: binding, SnapshotKeySHA256: key, SelectedDescriptorSHA256: strings.Repeat("a", 64)}
	c.CompiledExecute = &selector
	c.RequestDigest, err = runtime.SnapshotJobDigest("execute", binding, selector.SelectedDescriptorSHA256)
	if err != nil {
		t.Fatal(err)
	}
	return c, selector
}
func TestCodePlatformCompiledGrantUsesOriginalExecuteRelation(t *testing.T) {
	valid, selector := platformCompiledFixture(t)
	if valid.Validate() != nil || valid.RequestDigest == valid.PreparedFingerprint {
		t.Fatal("original compiled relation refused or collapsed to plain")
	}
	for _, tc := range []struct {
		name   string
		change func(*PlatformGrantClaims, *PlatformCompiledExecute)
	}{
		{"missing compiled selector", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) { c.CompiledExecute = nil }},
		{"prepared digest masquerades as compiled request", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) { c.RequestDigest = c.PreparedFingerprint }},
		{"raw SHA masquerades as base fingerprint", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) {
			s.Binding.BasePreparedRequestSHA256 = c.PreparedSHA256
		}},
		{"different original descriptor", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) {
			s.SelectedDescriptorSHA256 = strings.Repeat("b", 64)
		}},
		{"different snapshot", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) {
			s.SnapshotKeySHA256 = strings.Repeat("c", 64)
		}},
		{"foreign project", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) { s.Binding.ProjectID++ }},
		{"different measured module", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) {
			s.Binding.AdapterSHA256 = strings.Repeat("d", 64)
		}},
		{"nonbroker profile", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) { s.Binding.PolicyRevision = "isolated-v1" }},
		{"Compile digest", func(c *PlatformGrantClaims, s *PlatformCompiledExecute) {
			c.RequestDigest, _ = runtime.SnapshotJobDigest("compile", s.Binding, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := valid
			selected := selector
			changed.CompiledExecute = &selected
			tc.change(&changed, &selected)
			if changed.Validate() == nil {
				t.Fatal("forged compiled/platform role admitted")
			}
		})
	}
	wire, err := Canonical(valid)
	if err != nil || !strings.Contains(string(wire), `"compiled_execute":{`) {
		t.Fatal("required selector disappeared")
	}
	plain, _ := Canonical(platformClaimsFixture())
	if !strings.Contains(string(plain), `"compiled_execute":null`) {
		t.Fatal("plain selector omission changed protocol")
	}
}
func TestCodePlatformCompiledRuntimeComparesSelectorValues(t *testing.T) {
	c, selected := platformCompiledFixture(t)
	copy := selected
	r := RetainedRuntime{Kind: "docker", RuntimeID: strings.Repeat("c", 64), OwnerEpoch: 3, BindingSHA256: c.BindingSHA256, PreparedSHA256: c.PreparedSHA256, PreparedFingerprint: c.PreparedFingerprint, CompiledExecute: &copy, PolicySHA256: c.PolicySHA256, MaxCalls: c.MaxCalls, MaxTotalBytes: c.MaxTotalBytes, Lifecycle: "dispatched"}
	if !r.Matches(c) || r.CompiledExecute == c.CompiledExecute {
		t.Fatal("equal separately decoded selector refused")
	}
	copy.SelectedDescriptorSHA256 = strings.Repeat("d", 64)
	if r.Matches(c) {
		t.Fatal("substituted descriptor admitted")
	}
	r.CompiledExecute = nil
	if r.Matches(c) {
		t.Fatal("plain runtime substituted for compiled original")
	}
}
