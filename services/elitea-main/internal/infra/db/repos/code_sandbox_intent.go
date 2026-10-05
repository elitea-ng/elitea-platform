package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"time"

	recoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CodeDefinitionSourceReader is sealed to the Main repository package. The only
// production adapter consumes the sole capture/registered-family owner after
// CodeIntentRepository has locked and authenticated current access.
type CodeDefinitionSourceReader interface {
	readOriginalCodeSource(context.Context, sqlExecutor, string, uint64, codeAccess, json.RawMessage, string, string, string) (scope.SourceDefinition, error)
}

type codeOwnerEvidenceReader interface {
	Read(context.Context, code.GrantClaims, code.SignedGrant) ([]byte, error)
}

type CodeIntentRepository struct {
	shared      sharedStore
	permissions recoveryPermissionCheck
	signer      *storage.CodeOwnerGrantSigner
	owner       codeOwnerEvidenceReader
	sources     CodeDefinitionSourceReader
	snapshots   runtime.RustSnapshotIndex
	profiles    *runtime.RustSnapshotProfiles
	workspace   storage.OriginalCodeWorkspaceVerifier
	broker      storage.OriginalCodeBrokerVerifier
}

func NewCodeIntentRepository(pool *pgxpool.Pool, signer *storage.CodeOwnerGrantSigner, owner *storage.CodeOwnerClient, sources CodeDefinitionSourceReader, index runtime.RustSnapshotIndex, profiles *runtime.RustSnapshotProfiles) (*CodeIntentRepository, error) {
	if signer == nil || owner == nil || sources == nil {
		return nil, code.ErrRejected
	}
	shared, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &CodeIntentRepository{shared: shared, permissions: checkRecoveryPermission, signer: signer, owner: owner, sources: sources, snapshots: index, profiles: profiles}, nil
}

type codeAccess struct {
	tenant, actor, response, inputBundle, peer, desired, mode string
	project, projection, actorID                              int64
	attempt, epoch                                            uint64
	input, digest                                             []byte
	now, lease, deadline                                      time.Time
}

const codeAccessSQL = `SELECT j.tenant_id,j.resource_project_id,j.projection_project_id,j.actor_id,binding.client_message_id,j.input_bundle_id,
 c.workload_identity,c.claim_attempt,c.lease_epoch,j.desired_state,c.recovery_mode,e.content_bytes,e.content_digest,
 clock_timestamp(),c.lease_expires_at,command.deadline
 FROM elitea_runtime.execution_jobs j
 JOIN elitea_runtime.execution_claims c USING(execution_id,generation)
 JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id=c.workload_session_id AND ws.workload_identity=c.workload_identity AND ws.producer_id=c.producer_id
 JOIN elitea_runtime.command_outbox command USING(execution_id,generation)
 JOIN elitea_runtime.agent_execution_jobs binding USING(execution_id,generation)
 JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=binding.input_bundle_id AND e.entry_id=binding.request_entry_id
 JOIN centry.project project ON project.id=j.resource_project_id
 WHERE c.claim_id=$1 AND c.execution_id=$2 AND c.generation=$3 AND c.workload_identity=$4 AND c.fence_token=$5
 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()
 AND ws.issued_at<=clock_timestamp() AND ws.expires_at>clock_timestamp() AND ws.revoked_at IS NULL
 AND j.state='RUNNING' AND j.invocation_state='MAY_HAVE_STARTED' AND j.desired_state=$6 AND project.suspended=FALSE
 AND j.tenant_id=j.resource_project_id::text AND j.projection_project_id=j.resource_project_id
 AND j.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1') AND binding.capability_id=j.capability_id
 AND command.retired_at IS NULL AND command.authority_granted_at IS NOT NULL AND command.deadline>clock_timestamp()
 AND binding.input_bundle_id=j.input_bundle_id AND e.semantic_role='agent.execution_request' AND e.content_size=octet_length(e.content_bytes) AND e.content_size<=8388608
 AND NOT EXISTS(SELECT 1 FROM elitea_runtime.output_inbox terminal WHERE terminal.execution_id=j.execution_id AND terminal.generation=j.generation)
 FOR UPDATE OF j,c,ws,command`

func (r *CodeIntentRepository) lockAccess(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, desired string) (codeAccess, error) {
	peer, err := workloadidentity.Certificate(claim.PeerCertificate)
	if err != nil {
		return codeAccess{}, storage.ErrContentUnauthorized
	}
	return r.lockAccessIdentity(ctx, tx, claim, desired, peer)
}

// The identity-only entry is private and is used solely after sealed Supervisor
// read-grant verification; ordinary content claims always require their TLS peer.
func (r *CodeIntentRepository) lockAccessIdentity(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, desired, peer string) (codeAccess, error) {
	if r == nil || r.permissions == nil || tx == nil || !recovery.ValidExecutionID(claim.ExecutionID) || claim.Generation == 0 || claim.Generation > math.MaxInt64 || !recovery.ValidExecutionID(claim.ClaimID) || len(claim.FenceToken) != 32 || !code.Identity(peer) {
		return codeAccess{}, storage.ErrContentUnauthorized
	}
	var a codeAccess
	var err error
	for i := 0; i < 2; i++ {
		err = tx.QueryRow(ctx, codeAccessSQL, claim.ClaimID, claim.ExecutionID, int64(claim.Generation), peer, claim.FenceToken, desired).Scan(&a.tenant, &a.project, &a.projection, &a.actor, &a.response, &a.inputBundle, &a.peer, &a.attempt, &a.epoch, &a.desired, &a.mode, &a.input, &a.digest, &a.now, &a.lease, &a.deadline)
		if err != nil {
			return codeAccess{}, storage.ErrContentUnauthorized
		}
	}
	a.actorID, err = strconv.ParseInt(a.actor, 10, 64)
	if err != nil || a.actorID <= 0 || a.actorID > math.MaxInt32 || strconv.FormatInt(a.actorID, 10) != a.actor || a.project <= 0 || a.project > math.MaxInt32 || a.projection <= 0 || a.projection > math.MaxInt32 || a.peer != peer || a.inputBundle == "" || len(a.input) == 0 || len(a.digest) != 32 || !bytes.Equal(codeIntentDigestBytes(code.Digest(a.input)), a.digest) || a.attempt == 0 || a.epoch == 0 {
		return codeAccess{}, storage.ErrContentUnauthorized
	}
	if err = r.permissions(ctx, tx, recoveryapp.Selector{ProjectID: a.project, ActorUserID: a.actorID, ResponseMessageID: a.response}, "models.chat.messages.create"); err != nil {
		return codeAccess{}, storage.ErrContentUnauthorized
	}
	// Restored claims remain checkpoint-only. This admission never mints Begin/Invoke.
	if desired == "RUNNING" && a.mode != "NONE" {
		var authorized bool
		if a.mode == "AGENT_MODEL_CHECKPOINT" {
			err = tx.QueryRow(ctx, `SELECT model_checkpoint_digest IS NOT NULL FROM elitea_runtime.execution_claims WHERE claim_id=$1 AND execution_id=$2 AND generation=$3`, claim.ClaimID, claim.ExecutionID, int64(claim.Generation)).Scan(&authorized)
		} else if a.mode == "NODE_RECOVERY" {
			err = tx.QueryRow(ctx, `SELECT status='RESUMED' FROM elitea_runtime.node_recovery_visits WHERE execution_id=$1 AND generation=$2 ORDER BY created_at DESC,journal_revision DESC LIMIT 1 FOR SHARE`, claim.ExecutionID, int64(claim.Generation)).Scan(&authorized)
		} else {
			return codeAccess{}, storage.ErrContentUnauthorized
		}
		if err != nil || !authorized {
			return codeAccess{}, storage.ErrContentUnauthorized
		}
	}
	if desired == "SUSPENDED" && a.mode != "NODE_RECOVERY" {
		return codeAccess{}, storage.ErrContentUnauthorized
	}
	return a, nil
}
func codeIntentDigestBytes(v string) []byte { raw, _ := hex.DecodeString(v); return raw }
func grantEnd(a codeAccess) (int64, int64, error) {
	end := a.now.Add(30 * time.Second)
	if a.lease.Before(end) {
		end = a.lease
	}
	if a.deadline.Before(end) {
		end = a.deadline
	}
	if !end.After(a.now) {
		return 0, 0, code.ErrRejected
	}
	return a.now.UnixMilli(), end.UnixMilli(), nil
}

type originalCodeVisitRecord struct {
	Schema         string                       `json:"schema"`
	ExecutionID    string                       `json:"execution_id"`
	Generation     uint64                       `json:"original_generation"`
	Tenant         string                       `json:"tenant_id"`
	Project        int64                        `json:"project_id"`
	Projection     int64                        `json:"projection_project_id"`
	Actor          string                       `json:"actor_id"`
	InputSHA256    string                       `json:"root_input_sha256"`
	Activation     string                       `json:"activation_id"`
	Node           string                       `json:"node_id"`
	Thread         string                       `json:"graph_thread"`
	Step           uint64                       `json:"step"`
	Attempt        uint16                       `json:"attempt"`
	NodeDigest     string                       `json:"node_digest"`
	YAML           string                       `json:"owning_yaml_sha256"`
	Scope          json.RawMessage              `json:"saved_child_scope"`
	OwningSource   scope.SourceReference        `json:"owning_source_definition"`
	PreWorkspace   storage.CodePreparedMetadata `json:"pre_workspace"`
	PlatformClient bool                         `json:"platform_client"`
}

func codeVisitRef(v originalCodeVisitRecord) (code.OriginalVisitRef, []byte, error) {
	if v.OwningSource.Validate() != nil || v.OwningSource.YAMLSHA256 != v.YAML {
		return code.OriginalVisitRef{}, nil, code.ErrRejected
	}
	raw, err := code.Canonical(v)
	if err != nil {
		return code.OriginalVisitRef{}, nil, err
	}
	idRaw, _ := code.Canonical([]any{v.ExecutionID, v.Generation, v.Activation, v.Attempt})
	id := runtime.SnapshotHash("elitea.sandbox.original-code-visit.v1\x00", idRaw)
	return code.OriginalVisitRef{VisitID: id, Revision: 1, DigestSHA256: code.Digest(raw)}, raw, nil
}
func (r *CodeIntentRepository) originalSource(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, a codeAccess, v originalCodeVisitRecord, purpose string) (scope.SourceDefinition, error) {
	if r == nil || r.sources == nil || !originalCodePurpose(purpose) || v.ExecutionID != claim.ExecutionID || v.Generation != claim.Generation || v.Tenant != a.tenant || v.Project != a.project || v.Projection != a.projection || v.Actor != a.actor || v.InputSHA256 != code.Digest(a.input) {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	source, err := r.sources.readOriginalCodeSource(ctx, tx, claim.ExecutionID, claim.Generation, a, v.Scope, purpose, v.Thread, v.Node)
	if err != nil || source.Reference.Validate() != nil || source.ResourceProjectID != a.project || source.ActorID != a.actorID || source.Reference.YAMLSHA256 != v.YAML || code.Digest([]byte(source.Instructions)) != v.YAML {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	verified, err := scope.DecodeSourceWire(source.CanonicalWire, source.PreRedemptionVersion, source.Reference, a.project, a.actorID)
	if err != nil || verified.Instructions != source.Instructions {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	source = verified
	// A zero reference is accepted only before initial capture. Stored visit decode
	// requires a valid reference before this read, and exact reference replay.
	if v.OwningSource != (scope.SourceReference{}) && v.OwningSource != source.Reference {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	return source, nil
}
func (r *CodeIntentRepository) originalInstructions(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, a codeAccess, v originalCodeVisitRecord, purpose string) (string, error) {
	source, err := r.originalSource(ctx, tx, claim, a, v, purpose)
	if err != nil {
		return "", err
	}
	return source.Instructions, nil
}
func (r *CodeIntentRepository) originalPolicy(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, a codeAccess, v originalCodeVisitRecord, purpose string) ([]byte, error) {
	instructions, err := r.originalInstructions(ctx, tx, claim, a, v, purpose)
	if err != nil {
		return nil, err
	}
	var policy []byte
	if bytes.Equal(bytes.TrimSpace(v.Scope), []byte("null")) {
		policy, err = storage.OriginalRootSavedCodePolicy(instructions, v.YAML, v.Node)
	} else {
		policy, err = storage.OriginalSavedCodePolicy(instructions, v.YAML, v.Node)
	}
	if err != nil {
		return nil, err
	}
	return policy, nil
}
func (r *CodeIntentRepository) declaration(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, a codeAccess, v originalCodeVisitRecord, purpose string) (storage.SavedCodeDeclaration, error) {
	policy, err := r.originalPolicy(ctx, tx, claim, a, v, purpose)
	if err != nil {
		return storage.SavedCodeDeclaration{}, err
	}
	var digest [32]byte
	raw := codeIntentDigestBytes(v.NodeDigest)
	if len(raw) != 32 {
		return storage.SavedCodeDeclaration{}, code.ErrRejected
	}
	copy(digest[:], raw)
	return storage.ReadSavedCodeDeclaration(policy, digest)
}
func (r *CodeIntentRepository) RegisterOriginalCodeVisit(ctx context.Context, claim storage.ContentClaim, request code.VisitRequest) (code.VisitResponse, error) {
	configuration, prepared, err := storage.ValidateOriginalCodeVisit(request)
	if err != nil || r == nil || r.shared == nil {
		return code.VisitResponse{}, code.ErrRejected
	}
	var out code.VisitResponse
	err = r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		a, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil {
			return err
		}
		v := originalCodeVisitRecord{Schema: "elitea.sandbox.original-code-visit-record.v1", ExecutionID: claim.ExecutionID, Generation: claim.Generation, Tenant: a.tenant, Project: a.project, Projection: a.projection, Actor: a.actor, InputSHA256: code.Digest(a.input), Activation: request.ActivationID, Node: request.NodeID, Thread: request.GraphThread, Step: request.Step, Attempt: request.Attempt, NodeDigest: request.NodeDigest, YAML: request.OwningYAMLSHA256, Scope: bytes.Clone(request.SavedChildScope)}
		// Resolve the capture-owner reference before saved-policy comparison. Runtime
		// version_details is never substituted for pre-redemption source authority.
		source, err := r.originalSource(ctx, tx, claim, a, v, "code_recovery")
		if err != nil {
			return err
		}
		v.OwningSource = source.Reference
		var policy []byte
		if bytes.Equal(bytes.TrimSpace(v.Scope), []byte("null")) {
			policy, err = storage.OriginalRootSavedCodePolicy(source.Instructions, v.YAML, v.Node)
		} else {
			policy, err = storage.OriginalSavedCodePolicy(source.Instructions, v.YAML, v.Node)
		}
		if err != nil {
			return err
		}
		declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, string(configuration))
		if err != nil || hex.EncodeToString(declaration.ConfigurationDigest[:]) != request.NodeDigest {
			return code.ErrRejected
		}
		v.PlatformClient = declaration.PlatformClient
		v.PreWorkspace, err = storage.MatchCodePrepared(declaration, configuration, prepared)
		if err != nil {
			return err
		}
		inputPolicy, err := storage.OriginalSavedCodeInputPolicy(source.Instructions, v.YAML, v.Node)
		if err != nil {
			return err
		}
		parsed, err := storage.ParseCodePreparedRequest(prepared)
		if err != nil || inputPolicy.ValidateInput(parsed.Input) != nil {
			return code.ErrRejected
		}
		ref, raw, err := codeVisitRef(v)
		if err != nil {
			return err
		}
		var otherVisits int64
		err = tx.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.original_code_visits WHERE execution_id=$1 AND generation=$2 AND visit_id<>$3`, claim.ExecutionID, int64(claim.Generation), ref.VisitID).Scan(&otherVisits)
		if err != nil || otherVisits >= 4096 {
			return code.ErrRejected
		}
		_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.original_code_visits(execution_id,generation,visit_id,visit_digest,activation_id,attempt,record_json,registered_claim_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, claim.ExecutionID, int64(claim.Generation), ref.VisitID, ref.DigestSHA256, v.Activation, int16(v.Attempt), raw, claim.ClaimID)
		if err != nil {
			return err
		}
		var saved, digest []byte
		err = tx.QueryRow(ctx, `SELECT record_json,visit_digest FROM elitea_runtime.original_code_visits WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 FOR UPDATE`, claim.ExecutionID, int64(claim.Generation), ref.VisitID).Scan(&saved, &digest)
		if err != nil || !bytes.Equal(saved, raw) || !bytes.Equal(digest, []byte(ref.DigestSHA256)) {
			return code.ErrRejected
		}
		out = code.VisitResponse{Schema: "elitea.sandbox.original-code-visit-response.v1", OriginalVisit: ref, ExecutionID: v.ExecutionID, OriginalGeneration: v.Generation, ActivationID: v.Activation, Attempt: v.Attempt, NodeDigest: v.NodeDigest, PreWorkspacePreparedSHA256: v.PreWorkspace.PreparedSHA256}
		return nil
	})
	return out, err
}
func (r *CodeIntentRepository) readVisit(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, a codeAccess, ref code.OriginalVisitRef, purpose string) (originalCodeVisitRecord, storage.SavedCodeDeclaration, error) {
	if !originalCodePurpose(purpose) || ref.Validate() != nil {
		return originalCodeVisitRecord{}, storage.SavedCodeDeclaration{}, code.ErrRejected
	}
	var raw, digest []byte
	err := tx.QueryRow(ctx, `SELECT record_json,visit_digest FROM elitea_runtime.original_code_visits WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 FOR UPDATE`, claim.ExecutionID, int64(claim.Generation), ref.VisitID).Scan(&raw, &digest)
	if err != nil || code.Digest(raw) != ref.DigestSHA256 || !bytes.Equal(digest, []byte(ref.DigestSHA256)) {
		return originalCodeVisitRecord{}, storage.SavedCodeDeclaration{}, code.ErrRejected
	}
	var v originalCodeVisitRecord
	if code.Decode(raw, &v, 16384) != nil {
		return v, storage.SavedCodeDeclaration{}, code.ErrRejected
	}
	expected, wire, err := codeVisitRef(v)
	if err != nil || expected != ref || !bytes.Equal(raw, wire) {
		return v, storage.SavedCodeDeclaration{}, code.ErrRejected
	}
	d, err := r.declaration(ctx, tx, claim, a, v, purpose)
	if err != nil || d.PlatformClient != v.PlatformClient {
		return v, d, code.ErrRejected
	}
	return v, d, nil
}
func (r *CodeIntentRepository) ReadOriginalCodeVisit(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, ref code.OriginalVisitRef, purpose string) (storage.OriginalCodeVisit, error) {
	a, err := r.lockAccess(ctx, tx, claim, "RUNNING")
	if err != nil {
		return storage.OriginalCodeVisit{}, err
	}
	v, d, err := r.readVisit(ctx, tx, claim, a, ref, purpose)
	if err != nil {
		return storage.OriginalCodeVisit{}, err
	}
	return originalVisitView(a, claim, ref, v, d), nil
}

func (r *CodeIntentRepository) resolveOriginalCompiledCodeExecute(ctx context.Context, a codeAccess, v originalCodeVisitRecord, dispatch string, binding runtime.RustSnapshotBinding, selected, bundle string) (*storage.OriginalCompiledCodeExecute, string, error) {
	if r.snapshots == nil || r.profiles == nil || !code.NonzeroDigest(selected) || binding.TenantID != a.tenant || int64(binding.ProjectID) != a.project || binding.Validate() != nil || r.profiles.Validate(binding, bundle) != nil {
		return nil, "", code.ErrRejected
	}
	key, err := binding.Key()
	if err != nil {
		return nil, "", code.ErrRejected
	}
	scope := runtime.SnapshotScope{TenantID: a.tenant, ProjectID: binding.ProjectID}
	original, found, err := r.snapshots.OriginalExecution(ctx, scope, key, runtime.SnapshotActivationKey(v.ExecutionID, dispatch), selected)
	if err != nil {
		return nil, "", code.ErrRejected
	}
	root := original.Root
	descriptor := original.DescriptorJSON
	if !found {
		candidate, err := r.snapshots.Ready(ctx, scope, key)
		if err != nil {
			return nil, "", code.ErrRejected
		}
		root = candidate.Root
		descriptor = candidate.DescriptorJSON
	}
	parsed, err := runtime.ParseRustSnapshotDescriptor(descriptor, root)
	if err != nil || root != selected || parsed.Binding != binding {
		return nil, "", code.ErrRejected
	}
	digest, err := runtime.SnapshotJobDigest("execute", binding, root)
	if err != nil {
		return nil, "", code.ErrRejected
	}
	return &storage.OriginalCompiledCodeExecute{Binding: binding, DescriptorSHA256: root, SnapshotKeySHA256: key, DependencyBundleSHA256: bundle}, digest, nil
}
func (r *CodeIntentRepository) compiledDigest(ctx context.Context, a codeAccess, v originalCodeVisitRecord, request code.IntentRequest, prepared []byte, plain string) (string, []byte, error) {
	// Plain selector bytes retain the original two nullable fields exactly.
	if request.CompiledBindingBase64URL == nil {
		selector, _ := code.Canonical([]any{request.CompiledBindingBase64URL, request.SelectedDescriptorSHA256})
		return plain, selector, nil
	}
	if request.SelectedDescriptorSHA256 == nil {
		return "", nil, code.ErrRejected
	}
	raw, err := code.DecodeBase64(*request.CompiledBindingBase64URL, runtime.SnapshotDescriptorLimit)
	if err != nil {
		return "", nil, err
	}
	binding, err := runtime.ParseRustSnapshotBinding(raw)
	if err != nil {
		return "", nil, code.ErrRejected
	}
	bundle, err := storage.MatchCodeSnapshotPrepared(binding, prepared)
	if err != nil {
		return "", nil, code.ErrRejected
	}
	_, digest, err := r.resolveOriginalCompiledCodeExecute(ctx, a, v, request.DispatchActivation, binding, *request.SelectedDescriptorSHA256, bundle)
	if err != nil {
		return "", nil, err
	}
	// The third compiled-only fact is derived from the actual admitted final
	// PreparedJob parser. It binds profile dependency measurements on later reads.
	selector, _ := code.Canonical([]any{request.CompiledBindingBase64URL, request.SelectedDescriptorSHA256, bundle})
	return digest, selector, nil
}
func (r *CodeIntentRepository) RegisterOriginalCodeIntent(ctx context.Context, claim storage.ContentClaim, request code.IntentRequest) (code.SignedGrant, error) {
	prepared, err := storage.ValidateOriginalCodeIntent(request)
	if err != nil || r == nil || r.shared == nil || !r.signer.AllowsSupervisorAudience(request.SupervisorAudience) {
		return code.SignedGrant{}, code.ErrRejected
	}
	var claims code.IntentClaims
	err = r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		a, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil {
			return err
		}
		v, declaration, err := r.readVisit(ctx, tx, claim, a, request.OriginalVisit, "code_recovery")
		if err != nil {
			return code.ErrRejected
		}
		if err = matchOriginalCodeDispatch(declaration, v.Activation, v.Attempt, request.DispatchActivation); err != nil {
			return err
		}
		metadata, err := storage.MatchFinalCodePrepared(declaration, v.PreWorkspace, prepared)
		if err != nil {
			return err
		}
		parsed, err := storage.ParseCodePreparedRequest(prepared)
		if err != nil || declaration.PlatformClient != (parsed.Broker != nil) {
			return code.ErrRejected
		}
		original := originalVisitView(a, claim, request.OriginalVisit, v, declaration)
		if parsed.Workspace != nil {
			if r.workspace == nil {
				return code.ErrRejected
			}
			if _, err = r.originalSource(ctx, tx, claim, a, v, "code_workspace"); err != nil {
				return err
			}
			if err = r.workspace.VerifyOriginalCodeWorkspace(ctx, codeIntentTransaction{tx}, claim, original, parsed); err != nil {
				return err
			}
		}
		if parsed.Broker != nil {
			if !declaration.PlatformClient || r.broker == nil {
				return code.ErrRejected
			}
			if _, err = r.originalSource(ctx, tx, claim, a, v, "platform_broker"); err != nil {
				return err
			}
			if err = r.broker.VerifyOriginalCodeBroker(ctx, codeIntentTransaction{tx}, claim, original, parsed); err != nil {
				return err
			}
		}

		digest, selector, err := r.compiledDigest(ctx, a, v, request, prepared, metadata.Fingerprint)
		if err != nil || digest != request.RequestDigest {
			return code.ErrRejected
		}
		binding := code.Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: "whole_code_execute", ExecutionID: v.ExecutionID, OriginalGeneration: v.Generation, ActivationID: v.Activation, NodeID: v.Node, GraphThread: v.Thread, Step: v.Step, Attempt: v.Attempt, DispatchActivation: request.DispatchActivation, JobKey: code.JobKey(v.ExecutionID, request.DispatchActivation), RequestDigest: request.RequestDigest, SupervisorAudience: request.SupervisorAudience, NodeDigest: v.NodeDigest, Language: metadata.Language, PreparedSHA256: metadata.PreparedSHA256, SourceSHA256: metadata.SourceSHA256, InputSHA256: metadata.InputSHA256}
		if binding.Validate() != nil {
			return code.ErrRejected
		}
		wire, _ := code.Canonical(binding)
		_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.original_code_intents(execution_id,generation,visit_id,dispatch_activation,binding_json,binding_digest,compiled_selector_json,registered_claim_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, v.ExecutionID, int64(v.Generation), request.OriginalVisit.VisitID, request.DispatchActivation, wire, code.Digest(wire), selector, claim.ClaimID)
		if err != nil {
			return err
		}
		var saved, savedSelector []byte
		err = tx.QueryRow(ctx, `SELECT binding_json,compiled_selector_json FROM elitea_runtime.original_code_intents WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 FOR UPDATE`, v.ExecutionID, int64(v.Generation), request.OriginalVisit.VisitID).Scan(&saved, &savedSelector)
		if err != nil || !bytes.Equal(saved, wire) || !bytes.Equal(selector, savedSelector) {
			return code.ErrRejected
		}
		// Bounded index lookup may outlive a lease. Revalidate after it, before commit/signature.
		current, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil || current.attempt != a.attempt || current.epoch != a.epoch || !bytes.Equal(current.digest, a.digest) {
			return storage.ErrContentUnauthorized
		}
		issued, expires, err := grantEnd(current)
		if err != nil {
			return err
		}
		fence := sha256.Sum256(claim.FenceToken)
		claims = code.IntentClaims{Schema: "elitea.sandbox.original-code-intent.v1", Purpose: "whole_code_execute", TenantID: v.Tenant, ProjectID: v.Project, ExecutionID: v.ExecutionID, OriginalGeneration: v.Generation, ClaimID: claim.ClaimID, ClaimAttempt: a.attempt, LeaseEpoch: a.epoch, FenceSHA256: hex.EncodeToString(fence[:]), ActivationID: v.Activation, NodeID: v.Node, GraphThread: v.Thread, Step: v.Step, Attempt: v.Attempt, NodeDigest: v.NodeDigest, DispatchActivation: binding.DispatchActivation, JobKey: binding.JobKey, RequestDigest: binding.RequestDigest, SupervisorAudience: binding.SupervisorAudience, SubmitterWorkloadIdentity: a.peer, Language: binding.Language, PreparedSHA256: binding.PreparedSHA256, SourceSHA256: binding.SourceSHA256, InputSHA256: binding.InputSHA256, IssuedAtMillis: issued, ExpiresAtMillis: expires}
		return nil
	})
	if err != nil {
		return code.SignedGrant{}, err
	}
	return r.signer.SignIntent(claims)
}

var _ storage.OriginalCodeIntentStore = (*CodeIntentRepository)(nil)

// WithOriginalCodeVisit keeps the authenticated current execution/claim and original visit
// locked through a bounded consumer transaction. Its callback may not mint execution grants.
func (r *CodeIntentRepository) WithOriginalCodeVisit(ctx context.Context, claim storage.ContentClaim, ref code.OriginalVisitRef, purpose string, apply func(context.Context, storage.CodeTransaction, storage.OriginalCodeVisit) error) error {
	if r == nil || r.shared == nil || apply == nil || purpose != "code_debug" && purpose != "code_workspace" && purpose != "platform_broker" && purpose != "code_recovery" {
		return code.ErrRejected
	}
	return r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		visit, err := r.ReadOriginalCodeVisit(ctx, tx, claim, ref, purpose)
		if err != nil {
			return err
		}
		if err = apply(ctx, codeIntentTransaction{tx}, visit); err != nil {
			return err
		}
		current, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil || current.attempt != visit.CurrentClaimAttempt || current.epoch != visit.LeaseEpoch {
			return storage.ErrContentUnauthorized
		}
		return nil
	})
}

type codeIntentTransaction struct{ tx sqlExecutor }

func (t codeIntentTransaction) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, query, args...)
}
func (t codeIntentTransaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, query, args...)
}

var _ storage.OriginalCodeVisitConsumer = (*CodeIntentRepository)(nil)

func (r *CodeIntentRepository) WithPreparedExtensions(workspace storage.OriginalCodeWorkspaceVerifier, broker storage.OriginalCodeBrokerVerifier) *CodeIntentRepository {
	if r != nil {
		r.workspace = workspace
		r.broker = broker
	}
	return r
}
func originalVisitView(a codeAccess, claim storage.ContentClaim, ref code.OriginalVisitRef, v originalCodeVisitRecord, d storage.SavedCodeDeclaration) storage.OriginalCodeVisit {
	return storage.OriginalCodeVisit{TenantID: a.tenant, ResourceProjectID: a.project, ProjectionProjectID: a.projection, ActorID: a.actorID, CurrentClaimAttempt: a.attempt, CurrentClaimID: claim.ClaimID, CurrentWorkloadIdentity: a.peer, LeaseEpoch: a.epoch, Reference: ref, ExecutionID: v.ExecutionID, OriginalGeneration: v.Generation, ActivationID: v.Activation, NodeID: v.Node, GraphThread: v.Thread, Step: v.Step, Attempt: v.Attempt, NodeDigest: v.NodeDigest, OwningYAMLSHA256: v.YAML, OwningSourceDefinition: v.OwningSource, PreWorkspacePreparedSHA256: v.PreWorkspace.PreparedSHA256, SourceSHA256: v.PreWorkspace.SourceSHA256, InputSHA256: v.PreWorkspace.InputSHA256, Language: v.PreWorkspace.Language, PreWorkspaceFingerprint: v.PreWorkspace.Fingerprint, ImageDigest: v.PreWorkspace.ImageDigest, PolicyRevision: v.PreWorkspace.PolicyRevision, TimeoutSeconds: v.PreWorkspace.TimeoutSeconds, SavedChildScope: bytes.Clone(v.Scope), Declaration: d}
}

func (r *CodeIntentRepository) AuthorizeCodeWorkspaceCompile(ctx context.Context, certificate *x509.Certificate, fence runtime.Fence, ref code.OriginalVisitRef, prepared []byte) (storage.CodeCompileVisitAccess, error) {
	if r == nil || r.shared == nil || r.workspace == nil || fence.Validate() != nil || ref.Validate() != nil {
		return storage.CodeCompileVisitAccess{}, code.ErrRejected
	}
	identity, err := workloadidentity.Certificate(certificate)
	if err != nil || identity != fence.WorkloadIdentity {
		return storage.CodeCompileVisitAccess{}, storage.ErrContentUnauthorized
	}
	job, err := storage.ParseCodePreparedRequest(prepared)
	if err != nil || job.Workspace == nil || job.Broker != nil && job.PolicyRevision != "cargo-broker-execute-v1" {
		return storage.CodeCompileVisitAccess{}, code.ErrRejected
	}
	const exactClaim = `SELECT c.claim_id FROM elitea_runtime.execution_claims c JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
 WHERE c.execution_id=$1 AND c.generation=$2 AND j.command_id=$3 AND c.workload_identity=$4 AND c.workload_session_id=$5 AND c.producer_id=$6 AND c.claim_attempt=$7 AND c.lease_epoch=$8 AND c.fence_token=$9
 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()`
	args := []any{fence.ExecutionID, int64(fence.Generation), fence.CommandID, identity, fence.WorkloadSessionID, fence.ProducerID, int64(fence.ClaimAttempt), int64(fence.LeaseEpoch), fence.Token[:]}
	// Discovery grants nothing. WithOriginalCodeVisit repeats the live authority
	// checks, then the callback joins the exact compiled fence while locks are held.
	var claimID string
	err = r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error { return tx.QueryRow(ctx, exactClaim, args...).Scan(&claimID) })
	if err != nil {
		return storage.CodeCompileVisitAccess{}, storage.ErrContentUnauthorized
	}
	claim := storage.ContentClaim{PeerCertificate: certificate, ExecutionID: fence.ExecutionID, Generation: fence.Generation, ClaimID: claimID, FenceToken: bytes.Clone(fence.Token[:])}
	var result storage.CodeCompileVisitAccess
	err = r.WithOriginalCodeVisit(ctx, claim, ref, "code_workspace", func(ctx context.Context, tx storage.CodeTransaction, visit storage.OriginalCodeVisit) error {
		native, ok := tx.(codeIntentTransaction)
		if !ok {
			return code.ErrRejected
		}
		var currentClaim string
		if tx.QueryRow(ctx, exactClaim, args...).Scan(&currentClaim) != nil || currentClaim != claimID || visit.CurrentClaimAttempt != fence.ClaimAttempt || visit.LeaseEpoch != fence.LeaseEpoch {
			return storage.ErrContentUnauthorized
		}
		if visit.Declaration.PlatformClient != (job.Broker != nil) {
			return code.ErrRejected
		}
		if _, err := storage.MatchFinalCodePrepared(visit.Declaration, storage.CodePreparedMetadata{Language: visit.Language, SourceSHA256: visit.SourceSHA256, InputSHA256: visit.InputSHA256, ImageDigest: visit.ImageDigest, PolicyRevision: visit.PolicyRevision, TimeoutSeconds: visit.TimeoutSeconds}, prepared); err != nil {
			return err
		}
		if err = r.workspace.VerifyOriginalCodeWorkspace(ctx, tx, claim, visit, job); err != nil {
			return err
		}
		access, err := r.lockAccess(ctx, native.tx, claim, "RUNNING")
		if err != nil || access.attempt != fence.ClaimAttempt || access.epoch != fence.LeaseEpoch {
			return storage.ErrContentUnauthorized
		}
		_, expiry, err := grantEnd(access)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(fence.Token[:])
		result = storage.CodeCompileVisitAccess{OriginalVisit: ref, ClaimID: claimID, ClaimAttempt: access.attempt, LeaseEpoch: access.epoch, FenceSHA256: bytes.Clone(digest[:]), ExpiresAtUnixMillis: expiry}
		return nil
	})
	return result, err
}

var _ storage.OriginalCodeWorkspaceCompileAuthorizer = (*CodeIntentRepository)(nil)

// Omitted recovery policy preserves the compiler migration's legacy first dispatch.
// Main pins that sealed Worker identity; it does not reconstruct NodeContext state.
func matchOriginalCodeDispatch(d storage.SavedCodeDeclaration, activation string, attempt uint16, dispatch string) error {
	if !code.NonzeroDigest(dispatch) {
		return code.ErrRejected
	}
	if len(d.Recovery) == 0 || bytes.Equal(bytes.TrimSpace(d.Recovery), []byte("null")) {
		if attempt != 1 {
			return code.ErrRejected
		}
		return nil
	}
	if dispatch != code.DispatchActivation(activation, attempt) {
		return code.ErrRejected
	}
	return nil
}
