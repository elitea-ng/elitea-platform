package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// ExecutionChildScopesRepository is the sole saved-child registry. Callers that
// already own an effect/recovery transaction use ReadRegisteredSavedChild.
type ExecutionChildScopesRepository struct {
	store    sharedStore
	catalog  storage.SavedChildCatalogSource
	purposes map[scope.Purpose]bool
}

func NewExecutionChildScopesRepository(pool *pgxpool.Pool, catalog storage.SavedChildCatalogSource, enabled []scope.Purpose) (*ExecutionChildScopesRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, scope.ErrUnavailable
	}
	purposes := map[scope.Purpose]bool{}
	for _, purpose := range enabled {
		if !purpose.Valid() || purposes[purpose] {
			return nil, scope.ErrDenied
		}
		purposes[purpose] = true
	}
	// Empty operator purposes leaves all resource/effect admission disabled.
	return &ExecutionChildScopesRepository{store, catalog, purposes}, nil
}

const savedChildClaimSQL = `SELECT e.content_bytes,j.resource_project_id,j.actor_id::bigint,j.tenant_id,j.projection_project_id,c.claim_id,c.lease_epoch,o.deadline
 FROM elitea_runtime.execution_claims c
 JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
 JOIN elitea_runtime.agent_execution_jobs a USING(execution_id,generation)
 JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id=c.workload_session_id AND ws.workload_identity=c.workload_identity AND ws.producer_id=c.producer_id
 JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=a.input_bundle_id AND e.entry_id=a.request_entry_id
 JOIN elitea_runtime.command_outbox o USING(execution_id,generation)
 WHERE c.claim_id=$1 AND c.execution_id=$2 AND c.generation=$3 AND c.workload_identity=$4 AND c.fence_token=$5
 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()
 AND ws.revoked_at IS NULL AND ws.issued_at<=clock_timestamp() AND ws.expires_at>clock_timestamp()
 AND j.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1')
 AND j.desired_state='RUNNING' AND j.state IN ('RUNNING','SUSPENDED') AND j.invocation_state='MAY_HAVE_STARTED'
 AND o.deadline>clock_timestamp() AND e.semantic_role='agent.execution_request' AND e.content_size<=8388608
 FOR SHARE OF c,j,ws,e`

func readSavedChildClaim(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim) (storage.HTTPActionInput, error) {
	identity, err := workloadidentity.Certificate(claim.PeerCertificate)
	if err != nil || len(claim.FenceToken) != 32 {
		return storage.HTTPActionInput{}, scope.ErrDenied
	}
	var original storage.HTTPActionInput
	if tx.QueryRow(ctx, savedChildClaimSQL, claim.ClaimID, claim.ExecutionID, claim.Generation, identity, claim.FenceToken).Scan(&original.Payload, &original.ProjectID, &original.ActorID, &original.TenantID, &original.ProjectionProjectID, &original.ClaimID, &original.LeaseEpoch, &original.Deadline) != nil {
		return storage.HTTPActionInput{}, scope.ErrDenied
	}
	if original.ProjectID <= 0 || original.ActorID <= 0 || original.TenantID == "" || len(original.Payload) == 0 || len(original.Payload) > 8388608 {
		return storage.HTTPActionInput{}, scope.ErrDenied
	}
	return original, nil
}
func (repo *ExecutionChildScopesRepository) originalSavedChildRoot(ctx context.Context, tx sqlExecutor, original storage.HTTPActionInput, claim storage.ContentClaim) (scope.OwningDefinition, error) {
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(original.Payload, &input) != nil {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	source, err := repo.readOriginalRootSourceUnderOriginalAccess(ctx, tx, original)
	if err != nil {
		return scope.OwningDefinition{}, err
	}
	return scope.OwningDefinition{ExecutionID: claim.ExecutionID, Generation: claim.Generation, SourceReference: &source.Reference, OriginalSource: &source, ThreadID: httpapp.RootGraphThread(original.TenantID, original.ProjectID, original.ProjectionProjectID, input.GetThreadId()), TenantID: original.TenantID, ResourceProjectID: original.ProjectID, ProjectionProjectID: original.ProjectionProjectID, ActorID: original.ActorID, RootInputSHA256: scope.Digest(original.Payload), ApplicationID: source.Reference.ApplicationID, VersionID: source.Reference.VersionID, FrozenDefinitionSHA256: source.Reference.SourceDefinitionSHA256, YAMLSHA256: source.Reference.YAMLSHA256, Instructions: source.Instructions, PreRedemptionVersion: bytes.Clone(source.PreRedemptionVersion)}, nil
}
func savedChildMatchesClaim(wire scope.Wire, original storage.HTTPActionInput, claim storage.ContentClaim) bool {
	return wire.ExecutionID == claim.ExecutionID && wire.Generation == claim.Generation && wire.TenantID == original.TenantID && wire.ResourceProjectID == original.ProjectID && wire.ProjectionProjectID == original.ProjectionProjectID && wire.ActorID == original.ActorID && wire.RootInputSHA256 == scope.Digest(original.Payload)
}
func readSavedChildWire(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, reference scope.Reference) (scope.Wire, []byte, error) {
	if reference.Validate() != nil {
		return scope.Wire{}, nil, scope.ErrDenied
	}
	var raw []byte
	var digest string
	err := tx.QueryRow(ctx, `SELECT canonical_wire,digest_sha256 FROM elitea_runtime.execution_saved_child_scopes WHERE execution_id=$1 AND generation=$2 AND scope_id=$3 AND revision=$4 AND state='active' FOR SHARE`, claim.ExecutionID, claim.Generation, reference.ScopeID, reference.Revision).Scan(&raw, &digest)
	if err != nil {
		return scope.Wire{}, nil, scope.ErrDenied
	}
	wire, err := scope.DecodeWire(raw)
	if err != nil || wire.Reference() != reference || scope.Digest(raw) != digest {
		return scope.Wire{}, nil, scope.ErrDenied
	}
	return wire, raw, nil
}

// Caller tx is mandatory. Neither a selector nor a Worker checkpoint is a grant.
func (repo *ExecutionChildScopesRepository) ReadRegisteredSavedChild(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, reference scope.Reference, purpose scope.Purpose, owningThreadID string) (scope.OwningDefinition, error) {
	if repo == nil || tx == nil || !purpose.Valid() || !repo.purposes[purpose] {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	original, err := readSavedChildClaim(ctx, tx, claim)
	if err != nil {
		return scope.OwningDefinition{}, err
	}
	return repo.readRegistered(ctx, tx, claim, original, reference, &purpose, owningThreadID)
}

// This same-package port is called only after the actual Main owner has locked
// and verified its current access (Worker claim, or signed Supervisor job grant).
// It consumes that owner's original Main input inside the same transaction;
// it neither fabricates a Worker certificate nor creates a new read authority.
func (repo *ExecutionChildScopesRepository) readRegisteredSavedChildUnderOriginalAccess(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, original storage.HTTPActionInput, reference scope.Reference, purpose scope.Purpose, owningThreadID string) (scope.OwningDefinition, error) {
	if repo == nil || tx == nil || !purpose.Valid() || !repo.purposes[purpose] || executionID == "" || generation == 0 {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	return repo.readRegistered(ctx, tx, storage.ContentClaim{ExecutionID: executionID, Generation: generation}, original, reference, &purpose, owningThreadID)
}
func (repo *ExecutionChildScopesRepository) readRegistered(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, original storage.HTTPActionInput, reference scope.Reference, purpose *scope.Purpose, thread string) (scope.OwningDefinition, error) {
	root, err := repo.originalSavedChildRoot(ctx, tx, original, claim)
	if err != nil {
		return scope.OwningDefinition{}, err
	}
	wire, raw, err := readSavedChildWire(ctx, tx, claim, reference)
	if err != nil || !savedChildMatchesClaim(wire, original, claim) || purpose != nil && !wire.Allows(*purpose) {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	member, err := wire.Member(thread)
	if err != nil {
		return scope.OwningDefinition{}, err
	}
	ancestors := []scope.Reference{}
	seen := map[string]bool{wire.ScopeID: true}
	current := wire
	for current.Parent != nil {
		if len(ancestors) == scope.MaxAncestors || seen[current.Parent.ScopeID] {
			return scope.OwningDefinition{}, scope.ErrDenied
		}
		seen[current.Parent.ScopeID] = true
		parent, _, err := readSavedChildWire(ctx, tx, claim, *current.Parent)
		if err != nil || !savedChildMatchesClaim(parent, original, claim) {
			return scope.OwningDefinition{}, scope.ErrDenied
		}
		if _, err = parent.Member(current.Family.ParentThreadID); err != nil {
			return scope.OwningDefinition{}, scope.ErrDenied
		}
		ancestors = append(ancestors, *current.Parent)
		current = parent
	}
	if current.Family.ParentThreadID != root.ThreadID {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	var definition []byte
	if tx.QueryRow(ctx, `SELECT d.definition_bytes FROM elitea_runtime.execution_saved_child_definitions m JOIN elitea_runtime.execution_captured_definitions d USING(resource_project_id,application_id,version_id,definition_sha256) WHERE m.execution_id=$1 AND m.generation=$2 AND m.scope_id=$3 AND m.member_path=$4 AND m.graph_thread_id=$5 AND m.definition_sha256=$6 AND m.resource_project_id=$7 FOR SHARE OF m,d`, claim.ExecutionID, claim.Generation, reference.ScopeID, member.MemberPath, member.ThreadID, member.FrozenDefinitionSHA256, original.ProjectID).Scan(&definition) != nil {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	capturedSource, err := readCapturedRootSource(ctx, tx, *member.SourceReference, original.ProjectID, original.ActorID)
	if err != nil || capturedSource.Reference.ApplicationID != member.ApplicationID || capturedSource.Reference.VersionID != member.VersionID || capturedSource.Reference.YAMLSHA256 != member.YAMLSHA256 {
		return scope.OwningDefinition{}, scope.ErrDenied
	}
	owner, err := wire.OwningDefinition(raw, definition, thread, ancestors)
	if err != nil {
		return scope.OwningDefinition{}, err
	}
	owner.OriginalSource = &capturedSource
	return owner, nil
}

func (repo *ExecutionChildScopesRepository) RegisterSavedChild(ctx context.Context, claim storage.ContentClaim, request scope.Registration) (scope.Wire, error) {
	if repo == nil || repo.catalog == nil || request.Validate() != nil {
		return scope.Wire{}, scope.ErrDenied
	}
	var result scope.Wire
	err := repo.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx sqlExecutor) error {
		original, err := readSavedChildClaim(ctx, tx, claim)
		if err != nil {
			return err
		}
		// Registration dispatch requires RUNNING; SUSPENDED is read-only recovery.
		var running bool
		if tx.QueryRow(ctx, `SELECT state='RUNNING' FROM elitea_runtime.execution_jobs WHERE execution_id=$1 AND generation=$2`, claim.ExecutionID, claim.Generation).Scan(&running) != nil || !running {
			return scope.ErrDenied
		}
		parent, err := repo.originalSavedChildRoot(ctx, tx, original, claim)
		if err != nil {
			return err
		}
		if request.Parent != nil {
			parent, err = repo.readRegistered(ctx, tx, claim, original, *request.Parent, nil, request.Family.ParentThreadID)
			if err != nil {
				return err
			}
		}
		if parent.ThreadID != request.Family.ParentThreadID {
			return scope.ErrDenied
		}
		root, err := repo.originalSavedChildRoot(ctx, tx, original, claim)
		if err != nil || verifyCapturedSavedToolFamily(root, parent, request.Family) != nil {
			return scope.ErrDenied
		}
		candidate := scope.Wire{SchemaVersion: scope.Schema, Revision: 1, ExecutionID: claim.ExecutionID, Generation: claim.Generation, TenantID: original.TenantID, ResourceProjectID: original.ProjectID, ProjectionProjectID: original.ProjectionProjectID, ActorID: original.ActorID, RootInputSHA256: scope.Digest(original.Payload), Parent: request.Parent, Family: request.Family, Selected: request.Selected, RequestedPurposes: append([]scope.Purpose(nil), request.Purposes...)}
		for _, purpose := range request.Purposes {
			if repo.purposes[purpose] {
				candidate.Purposes = append(candidate.Purposes, purpose)
			}
		}
		candidate.ScopeID = candidate.OccurrenceID()
		var stored []byte
		existingErr := tx.QueryRow(ctx, `SELECT canonical_wire FROM elitea_runtime.execution_saved_child_scopes WHERE execution_id=$1 AND generation=$2 AND scope_id=$3 FOR UPDATE`, claim.ExecutionID, claim.Generation, candidate.ScopeID).Scan(&stored)
		if existingErr == nil {
			existing, err := scope.DecodeWire(stored)
			if err != nil || !savedChildMatchesClaim(existing, original, claim) || !sameSavedChildRegistration(existing, request) {
				return scope.ErrDenied
			}
			// Every ancestor is rechecked. A mutable version is never re-fetched on replay.
			if _, err = repo.readRegistered(ctx, tx, claim, original, existing.Reference(), nil, existing.Family.ChildThreadID); err != nil {
				return err
			}
			result = existing
			return nil
		}
		if !errors.Is(existingErr, pgx.ErrNoRows) {
			return scope.ErrUnavailable
		}
		captured, err := repo.catalog.CaptureSavedChildCatalog(ctx, claim, original, parent, request)
		if err != nil {
			return err
		}
		candidate.Members = captured.Members
		for i, member := range candidate.Members {
			source, err := readCapturedSavedChildSource(ctx, tx, original, claim, member.ApplicationID, member.VersionID, member.FrozenDefinitionSHA256)
			if err != nil {
				return err
			}
			candidate.Members[i].SourceReference = &source.Reference
		}
		candidate.Canonicalize()
		raw := candidate.CanonicalBytes()
		if _, err = scope.DecodeWire(raw); err != nil {
			return err
		}
		// Recheck expiry after bounded capture; no insert is authorized by an old read.
		if _, err = readSavedChildClaim(ctx, tx, claim); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.execution_saved_child_scopes(execution_id,generation,scope_id,revision,digest_sha256,canonical_wire,parent_scope_id,state,registration_claim_id,registration_lease_epoch) VALUES($1,$2,$3,1,$4,$5,$6,'active',$7,$8) ON CONFLICT(execution_id,generation,scope_id) DO NOTHING`, claim.ExecutionID, claim.Generation, candidate.ScopeID, candidate.Reference().DigestSHA256, raw, parentScopeID(request.Parent), original.ClaimID, original.LeaseEpoch)
		if err != nil || tag.RowsAffected() != 1 {
			return scope.ErrDenied
		}
		total := 0
		for _, member := range candidate.Members {
			definition := captured.Definitions[member.FrozenDefinitionSHA256]
			total += len(definition)
			if total > scope.MaxCatalogBytes || scope.FrozenDefinitionDigest(original.ProjectID, member.ApplicationID, member.VersionID, definition) != member.FrozenDefinitionSHA256 {
				return scope.ErrDenied
			}
			if err = captureMainDefinition(ctx, tx, original.ProjectID, member.ApplicationID, member.VersionID, member.FrozenDefinitionSHA256, definition); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.execution_saved_child_definitions(execution_id,generation,scope_id,member_path,graph_thread_id,resource_project_id,application_id,version_id,definition_sha256,yaml_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, claim.ExecutionID, claim.Generation, candidate.ScopeID, member.MemberPath, member.ThreadID, original.ProjectID, member.ApplicationID, member.VersionID, member.FrozenDefinitionSHA256, member.YAMLSHA256); err != nil {
				return scope.ErrUnavailable
			}
		}
		result = candidate
		return nil
	})
	return result, err
}
func parentScopeID(reference *scope.Reference) any {
	if reference == nil {
		return nil
	}
	return reference.ScopeID
}
func sameSavedChildRegistration(wire scope.Wire, request scope.Registration) bool {
	expected := scope.Registration{SchemaVersion: wire.SchemaVersion, Parent: wire.Parent, Family: wire.Family, Selected: wire.Selected, Purposes: wire.RequestedPurposes}
	for _, member := range wire.Members {
		expected.Members = append(expected.Members, scope.RequestedMember{MemberPath: member.MemberPath, ThreadID: member.ThreadID, FrozenDefinitionSHA256: member.FrozenDefinitionSHA256})
	}
	// Lists are canonicalized on both sides; nothing outside exact membership is accepted.
	canonical := func(value scope.Registration) []byte {
		probe := scope.Wire{Family: value.Family, Purposes: value.Purposes}
		probe.Canonicalize()
		value.Family = probe.Family
		value.Purposes = probe.Purposes
		// Requested member order is immaterial; member bytes and names are not.
		sort.Slice(value.Members, func(i, j int) bool { return value.Members[i].MemberPath < value.Members[j].MemberPath })
		raw, _ := json.Marshal(value)
		return raw
	}
	return bytes.Equal(canonical(expected), canonical(request))
}

// Only the authenticated registration path admits a capture. Consumers never
// turn these fields into permission; they read the immutable Main row instead.
func verifyCapturedSavedToolFamily(root, parent scope.OwningDefinition, family scope.OriginalFamily) error {
	if family.Kind != "agent_graph" && family.Kind != "agent_native" {
		return nil
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(family.OriginalLineageWireB64)
	if err != nil || len(raw) == 0 || len(raw) > 12288 || base64.StdEncoding.EncodeToString(raw) != family.OriginalLineageWireB64 {
		return scope.ErrDenied
	}
	var lineage struct {
		Schema    string `json:"schema"`
		Batch     string `json:"original_batch_event_id"`
		Ordinal   uint32 `json:"original_ordinal"`
		Call      string `json:"parent_call_id"`
		Tool      string `json:"tool_name"`
		Arguments string `json:"arguments_digest"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&lineage) != nil || decoder.Decode(new(any)) != io.EOF {
		return scope.ErrDenied
	}
	canonical, _ := json.Marshal(lineage)
	if !bytes.Equal(canonical, raw) || lineage.Schema != "elitea.pipeline.tool-call.v1" || lineage.Ordinal < 1 || lineage.Ordinal > 16 || lineage.Ordinal != family.Ordinal || lineage.Call != family.OriginalCallID || scope.Digest([]byte(lineage.Batch)) != family.OriginalBatchSHA256 || lineage.Arguments != family.InputSHA256 || lineage.Tool == "" || family.Kind == "agent_native" && lineage.Tool != family.NodeID {
		return scope.ErrDenied
	}
	// The declaration owns the selected application. A captured name must be the
	// generated selected-ID name, not another same-named editable alias.
	selected, err := storage.SavedChildEdge(parent.PreRedemptionVersion, family, parent.ResourceProjectID)
	if err != nil || lineage.Tool != storage.SavedApplicationCallName(selected[0], selected[1]) {
		return scope.ErrDenied
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("elitea.pipeline.static-tool-thread.v1\x00"))
	_, _ = hash.Write([]byte(root.ThreadID))
	_, _ = hash.Write(raw)
	expected := root.ThreadID + "/static-v1:" + hex.EncodeToString(hash.Sum(nil))
	if family.ChildThreadID != expected {
		return scope.ErrDenied
	}
	return nil
}

func (repo *ExecutionChildScopesRepository) WithSavedChildCatalog(catalog storage.SavedChildCatalogSource) *ExecutionChildScopesRepository {
	if repo == nil {
		return nil
	}
	copy := *repo
	copy.catalog = catalog
	return &copy
}
