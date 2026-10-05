package storage

import (
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// VerifyCodeBrokerRequestIdentity adds no grant and performs no IO. The original
// selector owner must already verify strict prepared bytes, dependency bundle,
// trusted release profile and original selected descriptor before producing it.
func VerifyCodeBrokerRequestIdentity(intent RegisteredCodeIntent, broker RegisteredCodeBrokerBinding, selected *OriginalCompiledCodeExecute) error {
	b := intent.Binding
	if b.Validate() != nil || !code.NonzeroDigest(broker.PreparedFingerprint) || !code.NonzeroDigest(broker.PreparedSHA256) || b.PreparedSHA256 != broker.PreparedSHA256 || intent.PreparedFingerprint != broker.PreparedFingerprint {
		return code.ErrRejected
	}
	if !intent.Compiled {
		if selected != nil || b.RequestDigest != broker.PreparedFingerprint {
			return code.ErrRejected
		}
		return nil
	}
	if selected == nil || b.Language != "rust" || !code.NonzeroDigest(selected.DescriptorSHA256) {
		return code.ErrRejected
	}
	s := selected.Binding
	o := intent.Original
	if s.Validate() != nil || s.TenantID != o.TenantID || int64(s.ProjectID) != o.ResourceProjectID || s.BasePreparedRequestSHA256 != broker.PreparedFingerprint || s.SourceSHA256 != b.SourceSHA256 || s.SourceSHA256 != o.SourceSHA256 || s.ExecutionImageDigest != o.ImageDigest || s.CompilationImageDigest != o.ImageDigest || s.PolicyRevision != "cargo-broker-execute-v1" || s.PolicyRevision != o.PolicyRevision {
		return code.ErrRejected
	}
	key, err := s.Key()
	if err != nil || key != selected.SnapshotKeySHA256 || selected.DependencyBundleSHA256 != broker.DependencyBundleSHA256 {
		return code.ErrRejected
	}
	digest, err := runtime.SnapshotJobDigest("execute", s, selected.DescriptorSHA256)
	if err != nil || digest != b.RequestDigest || digest == broker.PreparedFingerprint {
		return code.ErrRejected
	}
	return nil
}
func sameCodeBrokerCompiledExecute(a, b *OriginalCompiledCodeExecute) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
