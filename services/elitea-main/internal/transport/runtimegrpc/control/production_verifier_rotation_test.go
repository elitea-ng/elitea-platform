package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestProductionVerifierSupportsKeyRotationOverlapAndExplicitRetirement(t *testing.T) {
	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &publicKeyResolverStub{keys: map[string]ed25519.PublicKey{
		"runtime-signing-old": oldPublic,
		"runtime-signing-new": newPublic,
	}}
	verifier := newProductionVerifier(t, resolver)
	raw := validRawWorkerCommand(t)
	oldEnvelope := productionSignedEnvelope(t, "runtime-signing-old", oldPrivate, raw)
	newEnvelope := productionSignedEnvelope(t, "runtime-signing-new", newPrivate, raw)

	if _, err := verifier.Verify(context.Background(), oldEnvelope); err != nil {
		t.Fatalf("old key during overlap: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), newEnvelope); err != nil {
		t.Fatalf("new key during overlap: %v", err)
	}

	// Retirement is an explicit keyring change. Once the old exact ID is no
	// longer resolvable, an immutable envelope signed by it fails closed while
	// newly signed envelopes continue to verify.
	delete(resolver.keys, "runtime-signing-old")
	if _, err := verifier.Verify(context.Background(), oldEnvelope); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("retired old key error = %v, want authentication failure", err)
	}
	if _, err := verifier.Verify(context.Background(), newEnvelope); err != nil {
		t.Fatalf("active new key failed after old-key retirement: %v", err)
	}

	wrongKey := productionSignedEnvelope(t, "runtime-signing-new", oldPrivate, raw)
	if _, err := verifier.Verify(context.Background(), wrongKey); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("signature made by the wrong key returned %v", err)
	}
}

func TestProductionVerifierAuthenticatesImmutableReplayButRejectsCrossCommandSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := newProductionVerifier(t, &publicKeyResolverStub{keys: map[string]ed25519.PublicKey{
		"runtime-signing-active": publicKey,
	}})
	raw := validRawWorkerCommand(t)
	envelope := productionSignedEnvelope(t, "runtime-signing-active", privateKey, raw)

	// Signature verification is intentionally stateless: bus redelivery of
	// the same immutable envelope authenticates again. Durable command digest,
	// claim, lease and fence state—not the signature verifier—own replay safety.
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := verifier.Verify(context.Background(), envelope); err != nil {
			t.Fatalf("immutable replay attempt %d: %v", attempt+1, err)
		}
	}

	changed := productionSignedEnvelope(t, "runtime-signing-active", privateKey, raw)
	changed.WorkerCommandBytes = append([]byte(nil), raw...)
	changed.WorkerCommandBytes[len(changed.WorkerCommandBytes)-1] ^= 0x01
	changedDigest := sha256.Sum256(changed.WorkerCommandBytes)
	changed.WorkerCommandDigest.Value = changedDigest[:]
	// Keep the original signature to model replaying it over different command
	// bytes while also repairing the unauthenticated digest field.
	changed.Signature = append([]byte(nil), envelope.Signature...)
	if _, err := verifier.Verify(context.Background(), changed); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("cross-command signature replay returned %v", err)
	}
}
