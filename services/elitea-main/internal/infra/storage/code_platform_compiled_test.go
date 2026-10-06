package storage

import (
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"strings"
	"testing"
)

func brokerCompiledFixture() (RegisteredCodeIntent, RegisteredCodeBrokerBinding, *OriginalCompiledCodeExecute) {
	digest := func(v string) string { return strings.Repeat(v, 64) }
	activation, dispatch := digest("1"), digest("2")
	binding := code.Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: "whole_code_execute", ExecutionID: "0123456789abcdef0123456789abcdef", OriginalGeneration: 1, ActivationID: activation, NodeID: "run", GraphThread: "root", Step: 1, Attempt: 1, DispatchActivation: dispatch, JobKey: code.JobKey("0123456789abcdef0123456789abcdef", dispatch), RequestDigest: digest("3"), SupervisorAudience: "spiffe://elitea/supervisor/one", NodeDigest: digest("4"), Language: "rust", PreparedSHA256: digest("5"), SourceSHA256: digest("6"), InputSHA256: digest("7")}
	broker := RegisteredCodeBrokerBinding{PreparedSHA256: binding.PreparedSHA256, PreparedFingerprint: digest("8"), DependencyBundleSHA256: digest("b"), Broker: CodePreparedPlatformClient{Revision: 1, PolicySHA256: digest("9"), MaxCalls: 32, MaxTotalBytes: 1048576}}
	selected := &OriginalCompiledCodeExecute{DescriptorSHA256: digest("a"), Binding: runtime.RustSnapshotBinding{Revision: 1, ReusePolicy: "snapshot_v1", TenantID: "tenant", ProjectID: 7, BasePreparedRequestSHA256: broker.PreparedFingerprint, SourceSHA256: binding.SourceSHA256, CompilationImageDigest: "sha256:" + digest("b"), ExecutionImageDigest: "sha256:" + digest("b"), Platform: "linux/arm64/gnu", Target: "aarch64-unknown-linux-gnu", PolicyRevision: "cargo-broker-execute-v1", CargoManifestSHA256: digest("c"), CargoLockSHA256: digest("d"), CargoConfigSHA256: digest("e"), VendorSHA256: digest("f"), ToolchainSHA256: digest("1"), AdapterSHA256: digest("2"), WrapperSHA256: digest("3"), CompilerFlagsSHA256: digest("4")}}
	selected.SnapshotKeySHA256, _ = selected.Binding.Key()
	selected.DependencyBundleSHA256 = broker.DependencyBundleSHA256
	binding.RequestDigest, _ = runtime.SnapshotJobDigest("execute", selected.Binding, selected.DescriptorSHA256)
	intent := RegisteredCodeIntent{Binding: binding, PreparedFingerprint: broker.PreparedFingerprint, Compiled: true, Original: OriginalCodeVisit{TenantID: "tenant", ResourceProjectID: 7, SourceSHA256: binding.SourceSHA256, ImageDigest: selected.Binding.ExecutionImageDigest, PolicyRevision: selected.Binding.PolicyRevision}}
	return intent, broker, selected
}
func TestCompiledBrokerOriginalSelectorKeepsRequestAndPreparedDistinct(t *testing.T) {
	intent, broker, selected := brokerCompiledFixture()
	if err := VerifyCodeBrokerRequestIdentity(intent, broker, selected); err != nil {
		t.Fatal(err)
	}
	if intent.Binding.RequestDigest == broker.PreparedFingerprint {
		t.Fatal("compiled and prepared identities collapsed")
	}
	for name, change := range map[string]func(*RegisteredCodeIntent, *RegisteredCodeBrokerBinding, *OriginalCompiledCodeExecute){
		"bundle": func(_ *RegisteredCodeIntent, b *RegisteredCodeBrokerBinding, _ *OriginalCompiledCodeExecute) {
			b.DependencyBundleSHA256 = strings.Repeat("9", 64)
		},
		"snapshot key": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.SnapshotKeySHA256 = strings.Repeat("9", 64)
		},
		"request": func(i *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, _ *OriginalCompiledCodeExecute) {
			i.Binding.RequestDigest = strings.Repeat("9", 64)
		},
		"raw prepared": func(_ *RegisteredCodeIntent, b *RegisteredCodeBrokerBinding, _ *OriginalCompiledCodeExecute) {
			b.PreparedSHA256 = strings.Repeat("9", 64)
		},
		"prepared fingerprint": func(_ *RegisteredCodeIntent, b *RegisteredCodeBrokerBinding, _ *OriginalCompiledCodeExecute) {
			b.PreparedFingerprint = strings.Repeat("9", 64)
		},
		"descriptor": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.DescriptorSHA256 = strings.Repeat("9", 64)
		},
		"source": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.SourceSHA256 = strings.Repeat("9", 64)
		},
		"project": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.ProjectID++
		},
		"tenant": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.TenantID = "foreign"
		},
		"profile": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.PolicyRevision = "isolated-v1"
		},
		"wrapper": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.WrapperSHA256 = strings.Repeat("9", 64)
		},
		"manifest": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.CargoManifestSHA256 = strings.Repeat("9", 64)
		},
		"image": func(_ *RegisteredCodeIntent, _ *RegisteredCodeBrokerBinding, s *OriginalCompiledCodeExecute) {
			s.Binding.ExecutionImageDigest = "sha256:" + strings.Repeat("9", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			i, b, s := brokerCompiledFixture()
			change(&i, &b, s)
			if VerifyCodeBrokerRequestIdentity(i, b, s) == nil {
				t.Fatal("substitution accepted")
			}
		})
	}
	if VerifyCodeBrokerRequestIdentity(intent, broker, nil) == nil {
		t.Fatal("missing original selector accepted")
	}
	plain := intent
	plain.Compiled = false
	plain.Binding.RequestDigest = broker.PreparedFingerprint
	if VerifyCodeBrokerRequestIdentity(plain, broker, nil) != nil {
		t.Fatal("plain original behavior refused")
	}
	if VerifyCodeBrokerRequestIdentity(plain, broker, selected) == nil {
		t.Fatal("plain job accepted compiled selector")
	}
}
