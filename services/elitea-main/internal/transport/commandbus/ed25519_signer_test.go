package commandbus

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestEd25519CommandSignerUsesProductionDomain(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewEd25519CommandSigner("runtime-signing-2026-07", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	command := []byte("canonical-worker-command")
	signature, err := signer.SignWorkerCommand(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	input, err := runtimedomain.Ed25519WorkerCommandSigningInput(command)
	if err != nil {
		t.Fatal(err)
	}
	if signature.Profile != runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_ED25519 || signature.KeyID != "runtime-signing-2026-07" || !ed25519.Verify(publicKey, input, signature.Value) {
		t.Fatal("signer did not emit a valid production signature")
	}
	if ed25519.Verify(publicKey, command, signature.Value) {
		t.Fatal("production signature was valid without domain separation")
	}
}

func TestEd25519CommandSignerRejectsInvalidKeyAndCancellation(t *testing.T) {
	if _, err := NewEd25519CommandSigner("", make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("empty key ID was accepted")
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewEd25519CommandSigner("key-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := signer.SignWorkerCommand(ctx, []byte("command")); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}
