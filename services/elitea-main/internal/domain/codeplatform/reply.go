package codeplatform

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
)

// Receipt contains identifiers only. Payloads remain in private encrypted content.
type Receipt struct {
	EffectID   string `json:"effect_id"`
	CallSHA256 string `json:"call_sha256"`
	State      string `json:"state"`
}
type Reply struct {
	Revision uint32          `json:"revision"`
	Sequence uint64          `json:"sequence"`
	Status   string          `json:"status"`
	Result   json.RawMessage `json:"result"`
	Receipt  *Receipt        `json:"receipt"`
}

func validStatus(status string) bool {
	switch status {
	case "ok", "not_found", "sharing_denied", "authorization_denied", "authentication_denied", "approval_required", "sensitive_rejected", "dependency_unavailable", "unsupported_operation", "invalid_resource", "invalid_frame", "resource_exhausted", "revision_conflict", "unknown_effect", "stopped", "lease_lost":
		return true
	default:
		return false
	}
}
func EncodeReply(reply Reply, payload []byte) ([]byte, error) {
	if reply.Revision != Revision || reply.Sequence == 0 || reply.Sequence > MaxCalls || !validStatus(reply.Status) || len(payload) > MaxChunk || len(reply.Result) > MaxReplyHeader {
		return nil, ErrFrame
	}
	if len(reply.Result) == 0 {
		reply.Result = json.RawMessage("null")
	}
	if validateJSON(reply.Result) != nil {
		return nil, ErrFrame
	}
	if receipt := reply.Receipt; receipt != nil {
		if !digestPattern.MatchString(receipt.EffectID) || !digestPattern.MatchString(receipt.CallSHA256) || (receipt.State != "committed" && receipt.State != "uncertain") {
			return nil, ErrFrame
		}
	}
	if reply.Status == "ok" && (reply.Receipt == nil || reply.Receipt.State != "committed") {
		return nil, ErrFrame
	}
	if reply.Status != "ok" && (string(reply.Result) != "null" || len(payload) != 0) {
		return nil, ErrFrame
	}
	header, err := json.Marshal(reply)
	if err != nil || len(header) > MaxReplyHeader {
		return nil, ErrFrame
	}
	if validateJSON(header) != nil {
		return nil, ErrFrame
	}
	frame := make([]byte, 8+len(header)+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(header)))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], header)
	copy(frame[8+len(header):], payload)
	return frame, nil
}

// DecodeCommittedReply validates exact journal-owned bytes before any delivery.
// Re-encoding requires the canonical bounded frame produced by EncodeReply.
func DecodeCommittedReply(frame []byte) (Reply, error) {
	if len(frame) < 8 || len(frame) > 8+MaxReplyHeader+MaxChunk {
		return Reply{}, ErrFrame
	}
	header := uint64(binary.BigEndian.Uint32(frame[:4]))
	payload := uint64(binary.BigEndian.Uint32(frame[4:8]))
	if header == 0 || header > MaxReplyHeader || payload > MaxChunk || 8+header+payload != uint64(len(frame)) {
		return Reply{}, ErrFrame
	}
	var reply Reply
	if json.Unmarshal(frame[8:8+int(header)], &reply) != nil {
		return Reply{}, ErrFrame
	}
	encoded, err := EncodeReply(reply, frame[8+int(header):])
	if err != nil || !bytes.Equal(encoded, frame) || reply.Receipt == nil || reply.Receipt.State != "committed" {
		return Reply{}, ErrFrame
	}
	return reply, nil
}
