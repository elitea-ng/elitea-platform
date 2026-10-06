package commandbus

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// Ed25519CommandSigner owns one immutable active signing key. Rotating the
// active key affects only newly prepared outbox envelopes: previously prepared
// bytes retain their original key ID and signature and must remain verifiable
// until every such command has settled or reached its durable deadline.
type Ed25519CommandSigner struct {
	keyID      string
	privateKey ed25519.PrivateKey
}

func NewEd25519CommandSigner(keyID string, privateKey ed25519.PrivateKey) (*Ed25519CommandSigner, error) {
	if keyID == "" || len(keyID) > 256 || strings.ContainsAny(keyID, "\r\n\x00") || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("valid Ed25519 command-signing key and key ID are required")
	}
	return &Ed25519CommandSigner{
		keyID:      keyID,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
	}, nil
}

func (s *Ed25519CommandSigner) SignWorkerCommand(ctx context.Context, exactCommandBytes []byte) (Signature, error) {
	if err := ctx.Err(); err != nil {
		return Signature{}, err
	}
	input, err := runtimedomain.Ed25519WorkerCommandSigningInput(exactCommandBytes)
	if err != nil {
		return Signature{}, err
	}
	return Signature{
		Profile: runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_ED25519,
		KeyID:   s.keyID,
		Value:   ed25519.Sign(s.privateKey, input),
	}, nil
}

var _ CommandSigner = (*Ed25519CommandSigner)(nil)
