package noderecovery

import (
	"encoding/json"
)

const HTTPResultAudience = "elitea.runtime.http-action-receipt.v2"
const CodeResultAudience = "elitea.runtime.code-sandbox-whole-result.v1"

type ResultReference struct {
	ContentID             string `json:"content_id"`
	ImmutableVersion      string `json:"immutable_version"`
	DigestSHA256          string `json:"digest_sha256"`
	ByteLength            uint64 `json:"byte_length"`
	MediaType             string `json:"media_type"`
	RequiredGrantAudience string `json:"required_grant_audience"`
}

type OwnerProof struct {
	Schema             string           `json:"schema"`
	Kind               string           `json:"kind"`
	ExecutionID        string           `json:"execution_id"`
	Generation         uint64           `json:"generation"`
	ActivationID       string           `json:"activation_id"`
	Attempt            uint16           `json:"attempt"`
	ExpectedRevision   uint64           `json:"expected_revision"`
	EffectID           string           `json:"effect_id"`
	OwnerReceiptSHA256 string           `json:"owner_receipt_sha256"`
	ResultRef          *ResultReference `json:"result_ref,omitempty"`
	OwnerReceiptRef    *ResultReference `json:"owner_receipt_ref,omitempty"`
}

func DecodeOwnerProof(raw []byte, executionID string, generation uint64, receipt Receipt, action string) (OwnerProof, error) {
	var proof OwnerProof
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return OwnerProof{}, ErrInvalid
	}
	required := []string{"schema", "kind", "execution_id", "generation", "activation_id", "attempt", "expected_revision", "effect_id", "owner_receipt_sha256"}
	if _, ok := fields["result_ref"]; ok {
		required = append(required, "result_ref")
	}
	if _, ok := fields["owner_receipt_ref"]; ok {
		required = append(required, "owner_receipt_ref")
	}
	if strictDecode(raw, &proof, required) != nil || proof.Schema != "elitea.pipeline.node-recovery-owner-proof.v1" || proof.ExecutionID != executionID || proof.Generation != generation || proof.ActivationID != receipt.ActivationID || proof.Attempt != receipt.Attempt || proof.ExpectedRevision != receipt.JournalRevision || !ValidID(proof.EffectID) || !ValidID(proof.OwnerReceiptSHA256) {
		return OwnerProof{}, ErrInvalid
	}
	if receipt.ReplaySafety.EffectID != "" && proof.EffectID != receipt.ReplaySafety.EffectID {
		return OwnerProof{}, ErrInvalid
	}
	switch proof.Kind {
	case "verified_no_effect":
		if proof.ResultRef != nil || action == "resume_result" || receipt.ReplaySafety.Kind == "completed_external_effect" {
			return OwnerProof{}, ErrInvalid
		}
		if proof.OwnerReceiptRef != nil && !validOwnerReference(proof.OwnerReceiptRef, proof, CodeResultAudience) {
			return OwnerProof{}, ErrInvalid
		}
	case "committed_result":
		if action == "retry" || proof.ResultRef == nil || proof.OwnerReceiptRef != nil {
			return OwnerProof{}, ErrInvalid
		}
		ref := proof.ResultRef
		if !validOwnerReference(ref, proof, HTTPResultAudience) && !validOwnerReference(ref, proof, CodeResultAudience) {
			return OwnerProof{}, ErrInvalid
		}
	default:
		return OwnerProof{}, ErrInvalid
	}
	return proof, nil
}

func validOwnerReference(ref *ResultReference, proof OwnerProof, audience string) bool {
	return ref != nil && ref.ContentID == proof.EffectID && ref.ImmutableVersion == proof.OwnerReceiptSHA256 && ref.DigestSHA256 == proof.OwnerReceiptSHA256 && ValidID(ref.DigestSHA256) && ref.ByteLength > 0 && ref.ByteLength <= 1024*1024 && ref.MediaType == "application/json" && ref.RequiredGrantAudience == audience
}
