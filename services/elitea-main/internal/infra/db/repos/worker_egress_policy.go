package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workeregress"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// WorkerEgressSources reads what a worker command's public egress policy is
// assembled from (ADR-0044 decision 2, application/workeregress): the
// platform section (centry.platform_config `worker_egress`), the project's
// `worker_egress` configuration row in its tenant `configuration` table, and
// the command's input bundle entries (elitea_runtime.input_bundle_entries),
// whose toolkit settings name the hosts the run's toolkits reach, and, for a
// governed run, the target bindings of its credentials (elitea_identity).
type WorkerEgressSources struct{ pool *pgxpool.Pool }

var (
	_ workeregress.PlatformReader       = (*WorkerEgressSources)(nil)
	_ workeregress.ProjectReader        = (*WorkerEgressSources)(nil)
	_ workeregress.InputReader          = (*WorkerEgressSources)(nil)
	_ workeregress.GovernedTargetReader = (*WorkerEgressSources)(nil)
)

// NewWorkerEgressSources reads through pool.
func NewWorkerEgressSources(pool *pgxpool.Pool) (*WorkerEgressSources, error) {
	if pool == nil {
		return nil, errors.New("worker egress sources database is required")
	}
	return &WorkerEgressSources{pool: pool}, nil
}

// WorkerEgressSettings reads the platform section.
func (s *WorkerEgressSources) WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error) {
	return platformconfig.LoadWorkerEgressSettings(ctx, s.pool)
}

// WorkerEgressProjectEntries reads the project's own entries. A project with
// no row has none. An undecodable row is an error, never "none": the assembly
// then refuses the command instead of silently dropping the project's hosts.
// The entries are returned as stored; the assembly validates them.
func (s *WorkerEgressSources) WorkerEgressProjectEntries(ctx context.Context, projectID int64) ([]string, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT data FROM %s.configuration WHERE type = $1 ORDER BY id LIMIT 1`, schema),
		configurationapp.WorkerEgressConfigurationType).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the project setting: %w", err)
	}
	var setting struct {
		Allowlist []string `json:"allowlist"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return nil, fmt.Errorf("%w: %v", workeregress.ErrProjectSettingCorrupt, err)
		}
	}
	return setting.Allowlist, nil
}

// InputEntries reads the named entries of one input bundle.
func (s *WorkerEgressSources) InputEntries(ctx context.Context, inputBundleID string, entryIDs []string) (map[string][]byte, error) {
	rows, err := s.pool.Query(ctx, `
SELECT entry_id, content_bytes
FROM elitea_runtime.input_bundle_entries
WHERE input_bundle_id = $1 AND entry_id = ANY($2::text[])`, inputBundleID, entryIDs)
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the input entries: %w", err)
	}
	defer rows.Close()
	entries := make(map[string][]byte, len(entryIDs))
	for rows.Next() {
		var id string
		var content []byte
		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("worker egress: read an input entry: %w", err)
		}
		entries[id] = content
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker egress: read the input entries: %w", err)
	}
	return entries, nil
}

// GovernedEgressTargets reads the targets a governed run's credentials are
// bound to, the way the credential broker resolves them at redemption: the
// run's stored security context (elitea_runtime.execution_security_contexts),
// checked against the command's reference digest, names one frozen reference
// per credentialed toolkit. A reference's target binding is read directly; a
// toolkit frozen at its digest alone resolves, per operation, to the run's
// one usable grant, so every active `use` grant of the agent binding for that
// toolkit counts. Each target answers its canonical identity and, when set,
// its audience (the host a delegated token is used against).
func (s *WorkerEgressSources) GovernedEgressTargets(ctx context.Context, reference *runtimev1.ExecutionSecurityContextReferenceV1) ([]string, error) {
	contentID := reference.GetContentEntryId()
	if !securitycontext.IsContentID(contentID) || len(reference.GetDigest().GetValue()) != len(runtimedomain.Digest{}) {
		return nil, workeregress.ErrSecurityContextInvalid
	}
	executionID := strings.TrimPrefix(contentID, securitycontext.ContentIDPrefix)
	var contextBytes, digest []byte
	err := s.pool.QueryRow(ctx, `
SELECT context_bytes, digest
FROM elitea_runtime.execution_security_contexts
WHERE execution_id = $1`, executionID).Scan(&contextBytes, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workeregress.ErrSecurityContextInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the security context: %w", err)
	}
	computed := runtimedomain.SHA256(contextBytes)
	if !bytes.Equal(digest, reference.GetDigest().GetValue()) || !bytes.Equal(computed[:], digest) {
		return nil, workeregress.ErrSecurityContextInvalid
	}
	stored := &runtimev1.ExecutionSecurityContextV1{}
	if err := proto.Unmarshal(contextBytes, stored); err != nil {
		return nil, workeregress.ErrSecurityContextInvalid
	}
	var targetIDs []string
	var digestOnly []int64
	for _, frozen := range stored.GetCredentialReferences() {
		switch {
		case frozen.GetTargetBindingId() != "":
			if agentIdentityIsUUID(frozen.GetTargetBindingId()) {
				targetIDs = append(targetIDs, frozen.GetTargetBindingId())
			}
		case frozen.GetCredentialReferenceId() == "" && frozen.GetToolkitId() > 0:
			digestOnly = append(digestOnly, frozen.GetToolkitId())
		}
	}
	if len(targetIDs) == 0 && len(digestOnly) == 0 {
		return nil, nil
	}
	binding := stored.GetAgentBindingId()
	if !agentIdentityIsUUID(binding) {
		binding = ""
	}
	rows, err := s.pool.Query(ctx, `
SELECT t.target_identity, t.audience
FROM elitea_identity.target_bindings AS t
WHERE t.tenant_project_id = $1 AND t.id = ANY($2::uuid[])
UNION
SELECT t.target_identity, t.audience
FROM elitea_identity.credential_use_grants AS g
JOIN elitea_identity.target_bindings AS t
  ON t.id = g.target_binding_id AND t.tenant_project_id = g.tenant_project_id
WHERE $3::text <> '' AND g.tenant_project_id = $1 AND g.agent_binding_id = NULLIF($3, '')::uuid
  AND g.toolkit_id = ANY($4::bigint[]) AND g.state = 'active'
  AND (g.expires_at IS NULL OR g.expires_at > clock_timestamp())
  AND 'use' = ANY (g.capabilities)`, stored.GetTenantProjectId(), targetIDs, binding, digestOnly)
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the governed credential targets: %w", err)
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var identity, audience string
		if err := rows.Scan(&identity, &audience); err != nil {
			return nil, fmt.Errorf("worker egress: read a governed credential target: %w", err)
		}
		targets = append(targets, identity)
		if audience != "" {
			targets = append(targets, audience)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker egress: read the governed credential targets: %w", err)
	}
	return targets, nil
}
