package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type publicKeyResolverStub struct {
	keys      map[string]ed25519.PublicKey
	requested []string
}

func TestProductionVerifierSelectsCapabilitySpecificVersion(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &publicKeyResolverStub{keys: map[string]ed25519.PublicKey{"key-1": publicKey}}
	versions := map[string]string{
		"configuration.validate.v1": "1",
		"index.ingest.v1":           "2",
	}
	verifier, err := NewProductionCommandVerifier(ProductionVerifierConfig{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		ProtocolRevision:       "elitea.runtime.v1",
		CapabilityVersions:     versions,
		LimitsRevision:         "elitea.runtime.limits.conformance.v2",
		MaxWorkerCommandBytes:  32 * 1024,
		MaxInputManifestBytes:  64 * 1024,
		MaxStringBytes:         256,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	versions["index.ingest.v1"] = "1"
	indexCommand := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawIndexWorkerCommand(t), indexCommand); err != nil {
		t.Fatal(err)
	}
	indexCommand.CapabilityVersion = "2"
	indexV2, err := proto.MarshalOptions{Deterministic: true}.Marshal(indexCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(
		context.Background(),
		productionSignedEnvelope(t, "key-1", privateKey, indexV2),
	); err != nil {
		t.Fatalf("index v2 rejected: %v", err)
	}
	indexCommand.CapabilityVersion = "1"
	indexV1, err := proto.MarshalOptions{Deterministic: true}.Marshal(indexCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(
		context.Background(),
		productionSignedEnvelope(t, "key-1", privateKey, indexV1),
	); !errors.Is(err, ErrCommandIncompatible) {
		t.Fatalf("index v1 error=%v, want incompatible", err)
	}
	if _, err := verifier.Verify(
		context.Background(),
		productionSignedEnvelope(t, "key-1", privateKey, validRawWorkerCommand(t)),
	); err != nil {
		t.Fatalf("configuration v1 rejected: %v", err)
	}
}

func TestProductionVerifierRejectsAmbiguousCapabilityVersionConfiguration(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewProductionCommandVerifier(ProductionVerifierConfig{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		ProtocolRevision:       "elitea.runtime.v1",
		CapabilityVersion:      "1",
		CapabilityVersions:     map[string]string{"index.ingest.v1": "2"},
		LimitsRevision:         "elitea.runtime.limits.conformance.v2",
		MaxWorkerCommandBytes:  32 * 1024,
		MaxInputManifestBytes:  64 * 1024,
		MaxStringBytes:         256,
	}, &publicKeyResolverStub{keys: map[string]ed25519.PublicKey{"key-1": publicKey}})
	if err == nil {
		t.Fatal("scalar and per-capability versions must not be configured together")
	}
}

func (r *publicKeyResolverStub) ResolveEd25519PublicKey(_ context.Context, keyID string) (ed25519.PublicKey, error) {
	r.requested = append(r.requested, keyID)
	key, ok := r.keys[keyID]
	if !ok {
		return nil, errors.New("key not found")
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

func TestProductionVerifierAcceptsExactRotatedKeyIDAndRejectsTestProfile(t *testing.T) {
	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &publicKeyResolverStub{keys: map[string]ed25519.PublicKey{
		"runtime-signing-old": oldPublic,
		"runtime-signing-new": newPublic,
	}}
	verifier := newProductionVerifier(t, resolver)
	raw := validRawWorkerCommand(t)
	envelope := productionSignedEnvelope(t, "runtime-signing-old", oldPrivate, raw)
	if _, err := verifier.Verify(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if len(resolver.requested) != 1 || resolver.requested[0] != "runtime-signing-old" {
		t.Fatalf("resolved key IDs = %v, want exact old key", resolver.requested)
	}

	testOnly := signedEnvelope(raw)
	if _, err := verifier.Verify(context.Background(), testOnly); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("test-only profile error = %v, want authentication failure", err)
	}
	if len(resolver.requested) != 1 {
		t.Fatal("test-only profile reached the production key resolver")
	}
}

func TestProductionVerifierRejectsUnknownKeyTamperAndUndomainedSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &publicKeyResolverStub{keys: map[string]ed25519.PublicKey{"key-1": publicKey}}
	verifier := newProductionVerifier(t, resolver)
	raw := validRawWorkerCommand(t)

	unknown := productionSignedEnvelope(t, "unknown-key", privateKey, raw)
	if _, err := verifier.Verify(context.Background(), unknown); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("unknown-key error = %v, want authentication failure", err)
	}

	tampered := productionSignedEnvelope(t, "key-1", privateKey, raw)
	tampered.Signature[0] ^= 0xff
	if _, err := verifier.Verify(context.Background(), tampered); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("tamper error = %v, want authentication failure", err)
	}

	undomained := productionSignedEnvelope(t, "key-1", privateKey, raw)
	undomained.Signature = ed25519.Sign(privateKey, raw)
	if _, err := verifier.Verify(context.Background(), undomained); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("undomained error = %v, want authentication failure", err)
	}

	wrongLength := productionSignedEnvelope(t, "key-1", privateKey, raw)
	input, err := runtimedomain.Ed25519WorkerCommandSigningInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	domainLength := len(input) - 8 - len(raw)
	binary.BigEndian.PutUint64(input[domainLength:domainLength+8], uint64(len(raw)+1))
	wrongLength.Signature = ed25519.Sign(privateKey, input)
	if _, err := verifier.Verify(context.Background(), wrongLength); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("wrong-length-domain error = %v, want authentication failure", err)
	}
}

func newProductionVerifier(t *testing.T, resolver Ed25519PublicKeyResolver) *ProductionCommandVerifier {
	t.Helper()
	verifier, err := NewProductionCommandVerifier(ProductionVerifierConfig{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		ProtocolRevision:       "elitea.runtime.v1",
		CapabilityVersion:      "1",
		LimitsRevision:         "elitea.runtime.limits.conformance.v2",
		MaxWorkerCommandBytes:  32 * 1024,
		MaxInputManifestBytes:  64 * 1024,
		MaxStringBytes:         256,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func productionSignedEnvelope(t *testing.T, keyID string, privateKey ed25519.PrivateKey, raw []byte) *runtimev1.SignedWorkerCommandEnvelopeV1 {
	t.Helper()
	digest := sha256.Sum256(raw)
	input, err := runtimedomain.Ed25519WorkerCommandSigningInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.SignedWorkerCommandEnvelopeV1{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		SignatureProfile:       runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_ED25519,
		KeyId:                  keyID,
		WorkerCommandBytes:     append([]byte(nil), raw...),
		WorkerCommandDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     digest[:],
		},
		Signature: ed25519.Sign(privateKey, input),
	}
}
