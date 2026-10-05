package codeplatform

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
)

const CommittedReplySigningDomain = "elitea.code.platform-committed-reply.ed25519.v1\x00"
const MaxSignedHeader = 4096

// SigningAuthority rechecks the owning committed journal and current claim.
// A caller-constructed Record alone is never signing authority.
type SigningAuthority interface {
	VerifyCommittedCodeCall(context.Context, Admission, Record) error
}

type CommittedReplyClaims struct {
	Revision           uint8  `json:"revision"`
	KeyID              string `json:"key_id"`
	RetainedRuntimeID  string `json:"retained_runtime_id"`
	PreparedSHA256     string `json:"prepared_sha256"`
	PolicySHA256       string `json:"policy_sha256"`
	Sequence           uint64 `json:"sequence"`
	RequestFrameSHA256 string `json:"request_frame_sha256"`
	ReplyFrameSHA256   string `json:"reply_frame_sha256"`
	EffectID           string `json:"effect_id"`
}

func ReplyClaims(keyID string, job Job, record Record) (CommittedReplyClaims, error) {
	if job.Validate() != nil || !record.Committed() || record.EffectID != EffectID(job, record.Intent) || keyID == "" || len(keyID) > 256 || record.Intent.Sequence < 1 || record.Intent.Sequence > job.MaxCalls {
		return CommittedReplyClaims{}, ErrUnauthorized
	}
	return CommittedReplyClaims{Revision: 1, KeyID: keyID, RetainedRuntimeID: job.RuntimeID, PreparedSHA256: hex.EncodeToString(job.PreparedRequest[:]), PolicySHA256: hex.EncodeToString(job.Policy[:]), Sequence: record.Intent.Sequence, RequestFrameSHA256: hex.EncodeToString(record.Intent.Frame[:]), ReplyFrameSHA256: hex.EncodeToString(record.Response[:]), EffectID: record.EffectID}, nil
}

type SignedReply struct {
	Revision  uint8           `json:"revision"`
	KeyID     string          `json:"key_id"`
	Claims    json.RawMessage `json:"claims"`
	Signature string          `json:"signature"`
}

// SignedMailboxFrame keeps exact reply bytes outside JSON/base64 expansion.
func SignedMailboxFrame(signed SignedReply, exactReply []byte) ([]byte, error) {
	if signed.Revision != 1 || len(exactReply) < 8 || len(exactReply) > 8+MaxReplyHeader+MaxChunk {
		return nil, ErrConflict
	}
	header, err := json.Marshal(signed)
	if err != nil || len(header) > MaxSignedHeader {
		return nil, ErrConflict
	}
	frame := make([]byte, 8+len(header)+len(exactReply))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(header)))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(exactReply)))
	copy(frame[8:], header)
	copy(frame[8+len(header):], exactReply)
	return frame, nil
}
