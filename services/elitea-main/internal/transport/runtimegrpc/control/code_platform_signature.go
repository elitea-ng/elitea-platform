package control

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

// SignCommittedCodeCall reuses Main's existing key owner. Supervisor forwards it.
// No private key, bearer, or user-selected verification root leaves this owner.
func (issuer *SandboxGrantIssuer) SignCommittedCodeCall(ctx context.Context, authority domain.SigningAuthority, admission domain.Admission, record domain.Record) (domain.SignedReply, error) {
	if issuer == nil || ctx == nil || authority == nil || admission.Validate() != nil {
		return domain.SignedReply{}, domain.ErrUnauthorized
	}
	if err := authority.VerifyCommittedCodeCall(ctx, admission, record); err != nil {
		return domain.SignedReply{}, err
	}
	claims, err := domain.ReplyClaims(issuer.keyID, admission.Job, record)
	if err != nil {
		return domain.SignedReply{}, err
	}
	exact, err := json.Marshal(claims)
	if err != nil || len(exact) > domain.MaxSignedHeader {
		return domain.SignedReply{}, domain.ErrConflict
	}
	input := make([]byte, len(domain.CommittedReplySigningDomain)+8+len(exact))
	offset := copy(input, domain.CommittedReplySigningDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(exact)))
	copy(input[offset+8:], exact)
	if err := ctx.Err(); err != nil {
		return domain.SignedReply{}, err
	}
	return domain.SignedReply{Revision: 1, KeyID: issuer.keyID, Claims: exact, Signature: hex.EncodeToString(ed25519.Sign(issuer.key, input))}, nil
}
