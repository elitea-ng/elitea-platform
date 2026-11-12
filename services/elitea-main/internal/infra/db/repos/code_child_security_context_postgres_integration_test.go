package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// A code-sandbox toolkit call made inside a governed agent run inherits the
// run's binding and frozen credential references (issue 1557); a call that
// has no parent run (a user-started Test) binds no agent, so the broker
// denies it before any secret is read.
func TestPostgresCodeChildInheritsTheGovernedRunsBinding(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-code-child")
	admission := profile.GovernedAdmission(revision)
	now := time.Now().UTC()

	// The parent run's stored context: a binding and one frozen reference.
	parent := securitycontext.Context{
		PolicyRevisionID: revision.ID, PolicyDigest: runtimedomain.Digest(revision.Digest),
		TenantProjectID: 7, SubjectPrincipalRef: "5",
		AgentBindingID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a02", AgentBindingEpoch: 4,
		DefinitionDigest: runtimedomain.Digest{9}, IssuedAt: now,
		ApplicationID: 3, ApplicationVersionID: 4,
		ActorChain:      []securitycontext.ActorLink{{Kind: "user", Ref: "5"}},
		AgentSessionRef: "parent-session",
		CredentialReferences: []securitycontext.CredentialReference{{
			ToolkitID: 11, CredentialReferenceID: "ref-11", TargetBindingID: "target-11", Generation: 2,
			ToolkitDigest: runtimedomain.Digest{1},
		}},
	}
	encoded, ref, err := parent.Marshal("code-parent-exec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_security_contexts (execution_id, content_id, schema_revision, digest, context_bytes)
VALUES ($1, $2, $3, $4, $5)`, "code-parent-exec", ref.ContentID, ref.SchemaRevision, ref.Digest[:], encoded); err != nil {
		t.Fatal(err)
	}

	admit := func(executionID, parentID string) securitycontext.Reference {
		t.Helper()
		var out securitycontext.Reference
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			out, err = admitChildExecutionProfile(ctx, tx, pgxRows(tx), admission,
				admissionPin{executionID: executionID},
				securityContextFacts{tenantProjectID: 7, actorID: "5", admittedAt: now}, parentID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	stored := func(executionID string, reference securitycontext.Reference) securitycontext.Context {
		t.Helper()
		var bytes []byte
		if err := pool.QueryRow(ctx, `SELECT context_bytes FROM elitea_runtime.execution_security_contexts WHERE execution_id = $1`, executionID).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		message, err := securitycontext.Verify(bytes, reference)
		if err != nil {
			t.Fatal(err)
		}
		got, err := securitycontext.InheritedFrom(message, message.GetPolicyRevisionId(), runtimedomain.Digest(revision.Digest), 7, now)
		if err != nil {
			t.Fatal(err)
		}
		got.AgentSessionRef = message.GetAgentSessionRef()
		return got
	}

	inside := stored("code-child-inside", admit("code-child-inside", "code-parent-exec"))
	if inside.AgentBindingID != parent.AgentBindingID || inside.AgentBindingEpoch != 4 ||
		inside.DefinitionDigest != parent.DefinitionDigest || inside.ApplicationVersionID != 4 ||
		len(inside.CredentialReferences) != 1 || inside.CredentialReferences[0].CredentialReferenceID != "ref-11" {
		t.Fatalf("the child did not inherit the run's binding: %+v", inside)
	}
	if inside.AgentSessionRef != "" {
		t.Fatalf("the child must open its own agent session: %q", inside.AgentSessionRef)
	}
	// A re-admission returns the context stored first.
	if again := admit("code-child-inside", "code-parent-exec"); again.Digest != stored2Digest(t, ctx, pool, "code-child-inside") {
		t.Fatal("a re-admission changed the stored context")
	}

	// A call outside any run: no parent, no agent binding, no references.
	outside := stored("direct-test-call", admit("direct-test-call", ""))
	if outside.AgentBindingID != "" || len(outside.CredentialReferences) != 0 {
		t.Fatalf("a call outside a run bound an agent: %+v", outside)
	}

	// A parent from another project is not inherited: a clean governed
	// denial (the child binds no agent), not an admission fault.
	var foreign securitycontext.Reference
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		foreign, err = admitChildExecutionProfile(ctx, tx, pgxRows(tx), admission,
			admissionPin{executionID: "code-child-other-project"},
			securityContextFacts{tenantProjectID: 8, actorID: "5", admittedAt: now}, "code-parent-exec")
		return err
	})
	if err != nil {
		t.Fatalf("a parent of another project failed admission: %v", err)
	}
	var foreignBytes []byte
	if err := pool.QueryRow(ctx, `SELECT context_bytes FROM elitea_runtime.execution_security_contexts WHERE execution_id = $1`,
		"code-child-other-project").Scan(&foreignBytes); err != nil {
		t.Fatal(err)
	}
	foreignMessage, err := securitycontext.Verify(foreignBytes, foreign)
	if err != nil || foreignMessage.GetAgentBindingId() != "" || len(foreignMessage.GetCredentialReferences()) != 0 {
		t.Fatalf("a foreign parent was inherited: %+v %v", foreignMessage, err)
	}

	// A governed parent whose context row is missing fails loudly; a parent
	// that was never governed gives the child an unbound context.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return pinExecutionAdmission(ctx, tx, admission, admissionPin{executionID: "governed-parent-no-context"})
	}); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := admitChildExecutionProfile(ctx, tx, pgxRows(tx), admission,
			admissionPin{executionID: "code-child-orphan"},
			securityContextFacts{tenantProjectID: 7, actorID: "5", admittedAt: now}, "governed-parent-no-context")
		return err
	})
	if !errors.Is(err, ErrParentSecurityContextMissing) {
		t.Fatalf("a governed parent with no context: %v", err)
	}
	ungoverned := stored("code-child-ungoverned-parent", admit("code-child-ungoverned-parent", "never-admitted-parent"))
	if ungoverned.AgentBindingID != "" || len(ungoverned.CredentialReferences) != 0 {
		t.Fatalf("an ungoverned parent bound an agent: %+v", ungoverned)
	}
}

func stored2Digest(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, executionID string) runtimedomain.Digest {
	t.Helper()
	var digest []byte
	if err := pool.QueryRow(ctx, `SELECT digest FROM elitea_runtime.execution_security_contexts WHERE execution_id = $1`, executionID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return runtimedomain.Digest(digestArray(digest))
}
