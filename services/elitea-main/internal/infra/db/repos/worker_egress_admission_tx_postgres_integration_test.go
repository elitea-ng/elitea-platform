package repos

// The worker egress policy of a command prepared INSIDE its admission's
// uncommitted transaction (the synchronous tool call, its desktop remote
// twin): the resolver must read the input entries and the security context
// that transaction just wrote. Through a pool connection it cannot see them,
// and every such call in allowlist mode, or governed, was refused as
// EGRESS_POLICY_INVALID. It SKIPS without ELITEA_TEST_DATABASE_URL; each
// case asserts rows and hosts a skip could not have produced.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	toolkit "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workeregress"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type allowlistPlatform struct{}

func (allowlistPlatform) WorkerEgressSettings(context.Context) (platformconfig.WorkerEgressSettings, error) {
	return platformconfig.WorkerEgressSettings{Mode: platformconfig.WorkerEgressAllowlist, Allowlist: []string{}}, nil
}

// egressResolvingPreparer resolves the command's policy the way the
// commandbus producer does, inside the admission's prepare call.
type egressResolvingPreparer struct {
	resolver *workeregress.Resolver
	policy   *runtimev1.WorkerEgressPolicyV1
}

func (p *egressResolvingPreparer) PrepareToolkitCallTool(ctx context.Context, d toolkit.Dispatch) (executionapp.PreparedCommandEnvelope, error) {
	command := &runtimev1.WorkerCommandV1{
		ExecutionId: d.ExecutionID, Generation: d.Generation, ResourceProjectId: d.ResourceProjectID,
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: d.InputBundleID},
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{
			ToolkitType: d.ToolkitType, SettingsEntryId: d.SettingsEntryID,
		}},
	}
	policy, err := p.resolver.WorkerEgressPolicy(ctx, command)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	p.policy = policy
	raw := []byte("envelope-" + d.ExecutionID)
	return executionapp.PreparedCommandEnvelope{Bytes: raw, Digest: runtimedomain.SHA256(raw), SignatureProfile: 1, KeyID: "admission-key"}, nil
}

func TestPostgresToolCallAdmissionResolvesEgressInItsTransaction(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := workeregress.NewResolver(allowlistPlatform{}, sources, sources, sources)
	if err != nil {
		t.Fatal(err)
	}
	preparer := &egressResolvingPreparer{resolver: resolver}
	repository, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy(), preparer)
	if err != nil {
		t.Fatal(err)
	}
	request := toolRunSubmitRequest("egress-in-tx", `{"repo":"a"}`)
	request.Inputs.Settings = json.RawMessage(
		`{"id":19,"type":"github","toolkit_name":"gh","settings":{"github_configuration":{"base_url":"https://github.corp.example/api/v3"},"token":"secret-ref://toolkit/19"}}`)
	admitted, err := newToolRunAdmissionService(t, repository, "egress-in-tx").Submit(ctx, request)
	if err != nil {
		t.Fatalf("an allowlist tool call was not admitted: %v", err)
	}
	if !admitted.Outcome.Created || !slices.Equal(preparer.policy.GetAllow(), []string{"github.corp.example"}) {
		t.Fatalf("outcome=%+v policy=%v", admitted.Outcome, preparer.policy)
	}
	assertPostgresCount(t, ctx, pool, 1,
		`SELECT count(*) FROM elitea_runtime.command_outbox WHERE prepared_signed_envelope_bytes IS NOT NULL`)
}

// The sources read a governed run's security context and a bundle's entries
// through the transaction the context carries, and never see them through
// the pool before the commit.
func TestPostgresWorkerEgressSourcesReadThroughTheAdmissionTransaction(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	stored := &runtimev1.ExecutionSecurityContextV1{SchemaRevision: securitycontext.SchemaRevisionV1, TenantProjectId: 7}
	contextBytes, err := proto.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	digest := runtimedomain.SHA256(contextBytes)
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_security_contexts (execution_id, content_id, schema_revision, digest, context_bytes)
VALUES ('egress-tx-1', 'elitea.security-context/egress-tx-1', 'elitea.runtime.security-context.v1', $1, $2)`,
		digest[:], contextBytes); err != nil {
		t.Fatal(err)
	}
	reference := &runtimev1.ExecutionSecurityContextReferenceV1{
		ContentEntryId: securitycontext.ContentIDPrefix + "egress-tx-1",
		Digest:         &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: digest[:]},
		SchemaRevision: securitycontext.SchemaRevisionV1,
	}
	if _, err := sources.GovernedEgressTargets(ctx, reference); !errors.Is(err, workeregress.ErrSecurityContextInvalid) {
		t.Fatalf("the pool saw an uncommitted security context: %v", err)
	}
	inTx := withWorkerEgressTx(ctx, tx)
	if targets, err := sources.GovernedEgressTargets(inTx, reference); err != nil || len(targets) != 0 {
		t.Fatalf("in the transaction: %v %v", targets, err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO centry.project (id) VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-egress-tx', 'admission:bundle-egress-tx', 1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ('bundle-egress-tx', 'settings', '1', 'request', 'application/json',
    decode(repeat('cd', 32), 'hex'), 2, 'inline://settings', 'internal', 'worker', '{}'::bytea)`); err != nil {
		t.Fatal(err)
	}
	if read, err := sources.InputEntries(ctx, "bundle-egress-tx", []string{"settings"}); err != nil || len(read) != 0 {
		t.Fatalf("the pool saw an uncommitted entry: %v %v", read, err)
	}
	if read, err := sources.InputEntries(inTx, "bundle-egress-tx", []string{"settings"}); err != nil || string(read["settings"]) != "{}" {
		t.Fatalf("in the transaction: %v %v", read, err)
	}
}
