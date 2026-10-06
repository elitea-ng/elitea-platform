package repos

import (
	"bytes"
	"context"
	"encoding/json"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"google.golang.org/protobuf/proto"
)

// HTTPActionRecoveryProofProvider reads durable owner evidence on the caller's transaction.
type HTTPActionRecoveryProofProvider struct{}

func NewHTTPActionRecoveryProofProvider() *HTTPActionRecoveryProofProvider {
	return &HTTPActionRecoveryProofProvider{}
}

type httpRecoveryRecord struct {
	state, activation, effect, requestDigest, bindingDigest, policyDigest, outputBucket string
	request, receiptWire, input                                                         []byte
	receiptSHA                                                                          *string
	tenant                                                                              string
	project, projection                                                                 int64
}

// Only committed successful projection records justify a result proof.
// Missing or ambiguous records never justify a no-effect proof.
func (p *HTTPActionRecoveryProofProvider) VerifyNodeRecoveryEffect(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, journal recovery.Receipt, action string) (json.RawMessage, error) {
	record, proof, err := p.readCommitted(ctx, tx, executionID, generation, journal, action)
	_ = record
	if err != nil {
		return nil, err
	}
	return json.Marshal(proof)
}

// The caller authorizes the exact stored recovery action under its current claim.
// This source performs no effect dispatch, credential redemption, or artifact read.
func (p *HTTPActionRecoveryProofProvider) ReadNodeRecoveryHTTPResult(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, journal recovery.Receipt, authorizedProof json.RawMessage) ([]byte, error) {
	proof, err := recovery.DecodeOwnerProof(authorizedProof, executionID, generation, journal, "resume_result")
	if err != nil || proof.Kind != "committed_result" {
		return nil, app.ErrUnauthorized
	}
	record, current, err := p.readCommitted(ctx, tx, executionID, generation, journal, "resume_result")
	if err != nil {
		return nil, err
	}
	expected, _ := json.Marshal(current)
	if !bytes.Equal(expected, authorizedProof) {
		return nil, app.ErrUnauthorized
	}
	return bytes.Clone(record.receiptWire), nil
}

func (p *HTTPActionRecoveryProofProvider) readCommitted(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, journal recovery.Receipt, action string) (httpRecoveryRecord, recovery.OwnerProof, error) {
	if p == nil || tx == nil || !recovery.ValidExecutionID(executionID) || generation == 0 || journal.Validate() != nil || action != "reconcile" && action != "resume_result" {
		return httpRecoveryRecord{}, recovery.OwnerProof{}, app.ErrUnauthorized
	}
	effect := journal.ReplaySafety.EffectID
	if journal.ReplaySafety.Kind == "completed_external_effect" {
		effect = journal.ReplaySafety.ReceiptID
	}
	if !app.ValidDigest(effect) || journal.ReplaySafety.Kind != "unknown_external_effect" && journal.ReplaySafety.Kind != "completed_external_effect" {
		return httpRecoveryRecord{}, recovery.OwnerProof{}, app.ErrUnauthorized
	}
	var record httpRecoveryRecord
	err := tx.QueryRow(ctx, `SELECT h.state,h.activation_id,h.effect_id,h.request_digest,h.frozen_binding_digest,h.policy_digest,h.output_bucket,
 h.request_bytes,h.receipt_wire,h.receipt_sha256,e.content_bytes,j.tenant_id,j.resource_project_id,j.projection_project_id
 FROM elitea_runtime.execution_http_effects h
 JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
 JOIN elitea_runtime.agent_execution_jobs a USING(execution_id,generation)
 JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=a.input_bundle_id AND e.entry_id=a.request_entry_id
 WHERE h.execution_id=$1 AND h.generation=$2 AND h.effect_id=$3
 AND e.semantic_role='agent.execution_request' AND e.content_size<=8388608
 FOR SHARE OF h,e`, executionID, generation, effect).Scan(&record.state, &record.activation, &record.effect, &record.requestDigest, &record.bindingDigest, &record.policyDigest, &record.outputBucket, &record.request, &record.receiptWire, &record.receiptSHA, &record.input, &record.tenant, &record.project, &record.projection)
	if err != nil {
		return httpRecoveryRecord{}, recovery.OwnerProof{}, app.ErrUnauthorized
	}
	proof, err := committedHTTPRecoveryProof(executionID, generation, journal, record)
	if err != nil {
		return httpRecoveryRecord{}, recovery.OwnerProof{}, err
	}
	return record, proof, nil
}

func committedHTTPRecoveryProof(executionID string, generation uint64, journal recovery.Receipt, record httpRecoveryRecord) (recovery.OwnerProof, error) {
	if record.state != "completed" || record.receiptSHA == nil || !app.ValidDigest(*record.receiptSHA) || !app.ValidDigest(record.policyDigest) || len(record.receiptWire) == 0 || len(record.receiptWire) > 1024*1024 || app.Digest(record.receiptWire) != *record.receiptSHA || len(record.input) == 0 || len(record.input) > 8*1024*1024 || record.project <= 0 {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(record.input, &input) != nil {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	var application struct {
		ID        int64 `json:"id"`
		VersionID int64 `json:"version_id"`
		Version   struct {
			Instructions string             `json:"instructions"`
			Snapshot     app.FrozenSnapshot `json:"http_action_snapshot"`
		} `json:"version_details"`
	}
	if json.Unmarshal(input.Application, &application) != nil || journal.GraphThread != app.RootGraphThread(record.tenant, record.project, record.projection, input.GetThreadId()) {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	var frozen *app.FrozenNode
	for i := range application.Version.Snapshot.Nodes {
		node := &application.Version.Snapshot.Nodes[i]
		if node.NodeID == journal.NodeID {
			if frozen != nil {
				return recovery.OwnerProof{}, app.ErrUnauthorized
			}
			frozen = node
		}
	}
	if frozen == nil {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	invocation := app.Invocation{SchemaVersion: app.Schema, NodeID: journal.NodeID, ThreadID: journal.GraphThread, Step: journal.Step, RequestDigest: frozen.RequestDigest, BindingDigest: frozen.BindingDigest, RequestWireB64: frozen.RequestWireB64, Request: record.request}
	invocation.ActivationID = app.VisitID(invocation.ThreadID, invocation.NodeID, invocation.Step, app.BindingBytes(invocation.BindingDigest))
	expectedEffect := journal.ReplaySafety.EffectID
	if journal.ReplaySafety.Kind == "completed_external_effect" {
		expectedEffect = journal.ReplaySafety.ReceiptID
	}
	if record.activation != invocation.ActivationID || record.effect != expectedEffect || record.effect != app.EffectID(executionID, generation, invocation) || record.requestDigest != invocation.RequestDigest || record.bindingDigest != invocation.BindingDigest || application.Version.Snapshot.Verify(application.ID, application.VersionID, application.Version.Instructions, invocation) != nil {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	request, err := app.Parse(invocation)
	if err != nil {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	receipt, err := app.DecodeReceipt(record.receiptWire)
	if err != nil || receipt.State != "completed" || receipt.FailureCode != nil || receipt.Result == nil || receipt.ActivationID != invocation.ActivationID || receipt.EffectID != record.effect || receipt.RequestDigest != record.requestDigest || receipt.BindingDigest != record.bindingDigest || !validCommittedHTTPProjection(request, *receipt.Result, record.outputBucket, record.effect) {
		return recovery.OwnerProof{}, app.ErrUnauthorized
	}
	proof := recovery.OwnerProof{Schema: "elitea.pipeline.node-recovery-owner-proof.v1", Kind: "committed_result", ExecutionID: executionID, Generation: generation, ActivationID: journal.ActivationID, Attempt: journal.Attempt, ExpectedRevision: journal.JournalRevision, EffectID: record.effect, OwnerReceiptSHA256: *record.receiptSHA, ResultRef: &recovery.ResultReference{ContentID: record.effect, ImmutableVersion: *record.receiptSHA, DigestSHA256: *record.receiptSHA, ByteLength: uint64(len(record.receiptWire)), MediaType: "application/json", RequiredGrantAudience: "elitea.runtime.http-action-receipt.v2"}}
	return proof, nil
}

func validCommittedHTTPProjection(request app.Request, result app.Projection, bucket, effect string) bool {
	if result.ByteLength > request.Response.MaxBytes {
		return false
	}
	media := ""
	if result.ContentType != nil {
		media = *result.ContentType
	}
	var body []byte
	var artifact *app.Artifact
	switch result.Data.Kind {
	case "empty":
		if result.ByteLength != 0 || result.Data.Value != nil || result.Data.Reference != nil {
			return false
		}
	case "json":
		if request.Response.Mode != "json" || result.Data.Reference != nil {
			return false
		}
		var err error
		body, err = json.Marshal(result.Data.Value)
		if err != nil {
			return false
		}
	case "text":
		if request.Response.Mode != "text" || result.Data.Reference != nil {
			return false
		}
		value, ok := result.Data.Value.(string)
		if !ok {
			return false
		}
		body = []byte(value)
	case "artifact":
		artifact = result.Data.Reference
		if request.Response.Mode != "artifact" || result.Data.Value != nil || artifact == nil || bucket == "" || artifact.ByteLength != result.ByteLength || !app.ValidArtifact(*artifact, request.Response.MaxBytes) || artifact.ImmutableVersion != artifact.SHA256 || artifact.Reference != "artifact:"+bucket+":http-effects/"+effect+"/"+artifact.SHA256+".bin" {
			return false
		}
	default:
		return false
	}
	projected, failure := app.Project(request, result.Status, media, body, artifact)
	if failure != "" || projected == nil || projected.Data.Kind != result.Data.Kind {
		return false
	}
	encoded, err := json.Marshal(result)
	return err == nil && len(encoded) <= 512*1024
}
