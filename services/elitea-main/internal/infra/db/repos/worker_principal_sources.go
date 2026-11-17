package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// The run principal's bound (ADR-0044 Amendment 1, #2157;
// application/workerdelegation) reads the principal, the reachable toolkits
// and what the project's access rules hide through the same sources as the
// egress policy. Every statement is read-only.
var _ workerdelegation.PrincipalReader = (*WorkerEgressSources)(nil)

// ErrPrincipalSecurityContextInvalid is a governed command whose stored
// security context is missing or does not match its reference: the run's
// principal cannot be read, and the dispatch is retried.
var ErrPrincipalSecurityContextInvalid = errors.New("worker delegation: the run's security context is missing or does not match its reference")

// RunPrincipal answers who the command's run acts for. A governed run reads
// its stored security context (checked against the command's reference
// digest): a run under a mandate acts for the mandate and its subject user,
// any other for the actor. An ungoverned run acts for the command's actor
// (`principal_ref`, the user id), or for no user when it names none.
func (s *WorkerEgressSources) RunPrincipal(ctx context.Context, command *runtimev1.WorkerCommandV1) (workerdelegation.Principal, error) {
	reference := command.GetSecurityContextRef()
	if reference == nil {
		return workerdelegation.Principal{UserID: positiveUserID(command.GetPrincipalRef())}, nil
	}
	stored, err := s.storedSecurityContext(ctx, reference)
	if err != nil {
		return workerdelegation.Principal{}, err
	}
	// A run under a mandate acts for the mandate's subject user (none for an
	// autonomous mandate, which acts for the project); any other governed
	// run acts for its subject, else for the command's actor.
	principal := workerdelegation.Principal{
		Governed:  true,
		MandateID: stored.GetMandateId(),
		UserID:    positiveUserID(stored.GetSubjectPrincipalRef()),
	}
	for _, frozen := range stored.GetCredentialReferences() {
		if reference := frozen.GetCredentialReferenceId(); reference != "" {
			principal.FrozenCredentialReferences = append(principal.FrozenCredentialReferences, reference)
		}
	}
	if principal.MandateID == "" && principal.UserID == 0 {
		principal.UserID = positiveUserID(command.GetPrincipalRef())
	}
	return principal, nil
}

// storedSecurityContext reads and verifies the execution's security context
// (through the admission's transaction when ctx carries one).
func (s *WorkerEgressSources) storedSecurityContext(ctx context.Context, reference *runtimev1.ExecutionSecurityContextReferenceV1) (*runtimev1.ExecutionSecurityContextV1, error) {
	contentID := reference.GetContentEntryId()
	if !securitycontext.IsContentID(contentID) || len(reference.GetDigest().GetValue()) != len(runtimedomain.Digest{}) {
		return nil, ErrPrincipalSecurityContextInvalid
	}
	executionID := strings.TrimPrefix(contentID, securitycontext.ContentIDPrefix)
	var contextBytes, digest []byte
	err := s.querier(ctx).QueryRow(ctx, `
SELECT context_bytes, digest
FROM elitea_runtime.execution_security_contexts
WHERE execution_id = $1`, executionID).Scan(&contextBytes, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPrincipalSecurityContextInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the security context: %w", err)
	}
	computed := runtimedomain.SHA256(contextBytes)
	if !bytes.Equal(digest, reference.GetDigest().GetValue()) || !bytes.Equal(computed[:], digest) {
		return nil, ErrPrincipalSecurityContextInvalid
	}
	stored := &runtimev1.ExecutionSecurityContextV1{}
	if err := proto.Unmarshal(contextBytes, stored); err != nil {
		return nil, ErrPrincipalSecurityContextInvalid
	}
	return stored, nil
}

func positiveUserID(ref string) int64 {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// ReachableToolkits walks the saved agents and pipelines the seeds reach
// through attachments and answers every toolkit they hold, sorted. The walk
// reads versions level by level (CompositionSource, the save-time check's
// reader) and refuses a closure past workerdelegation.MaxClosureVersions.
func (s *WorkerEgressSources) ReachableToolkits(ctx context.Context, projectID int64, seeds workerdelegation.ClosureSeeds) ([]int64, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	toolkits := map[int64]bool{}
	for _, id := range seeds.ToolkitIDs {
		if id > 0 {
			toolkits[id] = true
		}
	}
	source := NewCompositionSource(s.pool, schema)
	seen := map[applicationcomposition.Identity]bool{}
	frontier := slices.Clone(seeds.Versions)
	for len(frontier) > 0 {
		var level []applicationcomposition.Identity
		for _, id := range frontier {
			if seen[id] {
				continue
			}
			seen[id] = true
			level = append(level, id)
		}
		if len(seen) > workerdelegation.MaxClosureVersions {
			return nil, workerdelegation.ErrPrincipalBoundTooLarge
		}
		if len(level) == 0 {
			break
		}
		if err := source.Prefetch(ctx, level); err != nil {
			return nil, fmt.Errorf("worker delegation: read the closure: %w", err)
		}
		versionIDs := make([]int64, 0, len(level))
		frontier = frontier[:0]
		for _, id := range level {
			version, ok, err := source.Version(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("worker delegation: read the closure: %w", err)
			}
			if !ok {
				continue
			}
			versionIDs = append(versionIDs, id.VersionID)
			for _, attachment := range version.Attachments {
				frontier = append(frontier, attachment.Identity)
			}
		}
		held, err := s.versionToolkitIDs(ctx, schema, versionIDs)
		if err != nil {
			return nil, err
		}
		for _, id := range held {
			toolkits[id] = true
		}
	}
	if len(toolkits) > workerdelegation.MaxPrincipalToolkits {
		return nil, workerdelegation.ErrPrincipalBoundTooLarge
	}
	out := make([]int64, 0, len(toolkits))
	for id := range toolkits {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

// versionToolkitIDs reads the toolkits (not attached agents) bound to the
// agent or pipeline versions.
func (s *WorkerEgressSources) versionToolkitIDs(ctx context.Context, schema string, versionIDs []int64) ([]int64, error) {
	if len(versionIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT tool.id::bigint
FROM %[1]s.entity_tool_mapping AS mapping
JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_type = 'agent'
  AND mapping.entity_version_id = ANY($1::bigint[])
  AND tool.type <> 'application'`, schema), versionIDs)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the closure's toolkits: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("worker delegation: read a closure toolkit: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker delegation: read the closure's toolkits: %w", err)
	}
	return ids, nil
}

// HiddenToolkits answers which of toolkitIDs the project's folder access
// hides from the user: the restrictive overlay on project RBAC
// (GetCurrentMCPToolkitVisibleToActor). A toolkit outside a folder, or in a
// folder with no `no_access` override for the user, stays usable. A project
// without the optional social folder access hides none; one with the
// overrides but not their dependencies is refused, as the toolkit reader
// refuses it. userID 0 hides none.
func (s *WorkerEgressSources) HiddenToolkits(ctx context.Context, projectID, userID int64, toolkitIDs []int64) ([]int64, error) {
	if userID <= 0 || len(toolkitIDs) == 0 {
		return nil, nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	name := strings.Trim(schema, `"`)
	var state int
	if err := s.pool.QueryRow(ctx, `
SELECT CASE
    WHEN to_regclass($1) IS NOT NULL
     AND to_regclass($2) IS NOT NULL
     AND to_regclass($3) IS NOT NULL THEN 1
    WHEN to_regclass($3) IS NULL THEN 0
    ELSE -1
END::integer`, name+".entity_folders", name+".social_folder_items", name+".folder_access_overrides").Scan(&state); err != nil {
		return nil, fmt.Errorf("worker delegation: probe the folder access tables: %w", err)
	}
	switch state {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, errors.New("worker delegation: the project's folder access is partially installed")
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT item.entity_id::bigint
FROM %[1]s.social_folder_items AS item
JOIN %[1]s.folder_access_overrides AS access ON access.folder_id = item.folder_id
WHERE item.entity IN ('toolkit', 'mcp')
  AND item.entity_id = ANY($1::bigint[])
  AND access.user_id = $2
  AND access.access_level = 'no_access'`, schema), toolkitIDs, userID)
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the hidden toolkits: %w", err)
	}
	defer rows.Close()
	var hidden []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("worker delegation: read a hidden toolkit: %w", err)
		}
		hidden = append(hidden, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker delegation: read the hidden toolkits: %w", err)
	}
	return hidden, nil
}

// UsableCredentialReferences answers the credential references an active,
// unexpired ADR-0039 `use` grant of the project lets a toolkit of toolkitIDs
// use, sorted. The broker still decides the exact grant (agent binding,
// target, scopes) at every call; this is the outer bound of what any caller
// could use. A database without the identity tables has none.
func (s *WorkerEgressSources) UsableCredentialReferences(ctx context.Context, projectID int64, toolkitIDs []int64) ([]string, error) {
	if len(toolkitIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT credential_reference_id::text
FROM elitea_identity.credential_use_grants
WHERE tenant_project_id = $1
  AND toolkit_id = ANY($2::bigint[])
  AND state = 'active'
  AND (expires_at IS NULL OR expires_at > clock_timestamp())
  AND 'use' = ANY (capabilities)
ORDER BY 1`, projectID, toolkitIDs)
	if err != nil {
		if undefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worker delegation: read the credential use grants: %w", err)
	}
	defer rows.Close()
	var references []string
	for rows.Next() {
		var reference string
		if err := rows.Scan(&reference); err != nil {
			return nil, fmt.Errorf("worker delegation: read a credential reference: %w", err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		if undefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worker delegation: read the credential use grants: %w", err)
	}
	return references, nil
}
