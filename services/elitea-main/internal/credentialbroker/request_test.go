package credentialbroker

import (
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// viewOf converts the frozen references in one pass, with the same answer
// per toolkit as securitycontext.CredentialReferenceFor: the first entry of a
// toolkit wins, and one whose digest is not a SHA-256 is not frozen.
func TestViewOfFreezesEveryReferenceLikeCredentialReferenceFor(t *testing.T) {
	sha := func(b byte) *runtimev1.DigestV1 {
		value := make([]byte, 32)
		value[0] = b
		return &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: value}
	}
	message := &runtimev1.ExecutionSecurityContextV1{}
	for id := int64(1); id <= securitycontext.MaxCredentialReferences; id++ {
		message.CredentialReferences = append(message.CredentialReferences, &runtimev1.GovernedCredentialReferenceV1{
			ToolkitId: id, CredentialReferenceId: "ref", TargetBindingId: "target", Generation: id, ToolkitDigest: sha(byte(id)),
		})
	}
	message.CredentialReferences[2].ToolkitDigest = &runtimev1.DigestV1{Value: []byte{1}}
	message.CredentialReferences = append(message.CredentialReferences,
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 2, Generation: 99, ToolkitDigest: sha(9)},
		&runtimev1.GovernedCredentialReferenceV1{ToolkitId: 3, Generation: 99, ToolkitDigest: sha(9)})

	view := viewOf(message)
	if len(view.frozen) != securitycontext.MaxCredentialReferences-1 {
		t.Fatalf("frozen = %d", len(view.frozen))
	}
	for id := int64(1); id <= securitycontext.MaxCredentialReferences+1; id++ {
		want, wantOK := securitycontext.CredentialReferenceFor(message, id)
		got, ok := view.frozen[id]
		if ok != wantOK || got != want {
			t.Fatalf("toolkit %d: %+v %v, want %+v %v", id, got, ok, want, wantOK)
		}
	}
	if view.frozen[2].Generation != 2 {
		t.Fatal("a later duplicate replaced the first entry")
	}
}
