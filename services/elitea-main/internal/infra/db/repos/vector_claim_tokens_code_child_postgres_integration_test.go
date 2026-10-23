package repos

// A Code node's toolkit_call child (toolkit.call_tool.v1 spawned from an agent
// turn's code) is under its PARENT's conversation rules (security review 6):
// it inherits the parent's conversation, taint and trigger origin, it is not
// conversation-unbound, and it reaches a memory toolkit only if the parent
// run was given it. Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	codeplatformdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestPostgresCodeToolkitChildInheritsItsParentsConversation(t *testing.T) {
	w := newEffectsWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project := w.project()
	const conversation = "77777777-7777-4777-8777-777777777777"

	// The parent agent turn, bound to a conversation, given memory toolkit 5.
	w.job("parent", project, "RUNNING")
	w.admit("parent")
	w.exec(`UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $2 WHERE execution_id = $1`, "parent", conversation)
	w.exec(`UPDATE elitea_runtime.execution_jobs SET memory_toolkit_ids = '{5}' WHERE execution_id = 'parent'`)
	// Its code calls a toolkit: the child run (toolkit.call_tool.v1).
	w.registration("parent", effectsHex("a"), effectsHex("b"), project)
	w.access("parent", project, "claim-parent", 1, 0x01)
	w.call("parent", effectsHex("a"), 1, "dispatching", effectID(1), "claim-parent")
	w.toolkitChild(effectID(1), "child", "RUNNING", project)
	w.exec(`UPDATE elitea_runtime.execution_jobs SET capability_id = 'toolkit.call_tool.v1', memory_toolkit_ids = '{5}' WHERE execution_id = 'child'`)

	store := NewVectorClaimTokens(w.pool)
	turn, err := store.executionConversation(ctx, "child", 1)
	if err != nil || turn.conversation != conversation || turn.root != "parent" || turn.origin != "manual" {
		t.Fatalf("the child's binding = %+v, %v; want the parent's conversation", turn, err)
	}

	// A token for the child's claim.
	childClaim := w.access("child", project, "claim-child", 1, 0x02)
	var fence runtimedomain.Fence
	fence.ExecutionID, fence.Generation, fence.WorkloadIdentity = "child", 1, effectsWorkload
	copy(fence.Token[:], childClaim.FenceToken)
	fence.ClaimAttempt, fence.LeaseEpoch, fence.CommandID = 1, 1, "command-child"
	w.exec(`INSERT INTO elitea_runtime.command_outbox (outbox_id, execution_id, generation, stream_name, resource_class, isolation_class, priority, deadline, limits_revision,
 prepared_signed_envelope_bytes, prepared_signed_envelope_digest, prepared_signature_profile, prepared_key_id, prepared_at, published_at, published_envelope_digest, authority_granted_at)
VALUES ('outbox-child', 'child', 1, 'commands', 'toolkit', 'project', 1, clock_timestamp()+interval '1 hour', 'limits', 'e'::bytea, sha256('e'::bytea), 1, 'key', clock_timestamp(), clock_timestamp(), sha256('e'::bytea), clock_timestamp())`)
	minted, err := store.Mint(ctx, VectorClaimTokenMint{
		Fence: fence, TokenSHA256: sha256.Sum256([]byte("bearer-child")),
		Capabilities: vectorClaimCapabilities, Sources: []string{"toolkit_index"},
		MemorySource: "memory", MaxLifetime: time.Hour,
	})
	if err != nil || !minted.Minted || !slices.Equal(minted.Sources, []string{"toolkit_index", "memory"}) {
		t.Fatalf("mint the child's token: %+v %v", minted, err)
	}
	facts := func(intent MemoryIntent) VectorClaimTokenFacts {
		t.Helper()
		got, err := store.Facts(ctx, sha256.Sum256([]byte("bearer-child")), intent)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if facts(MemoryIntentNone).ConversationUnbound {
		t.Fatal("a Code child of an agent turn is conversation-unbound")
	}
	// No conversation row in the tenant (deleted, or never there): a shared
	// write fails closed.
	if !facts(MemoryIntentProjectWrite).ProjectMemoryWriteRefused {
		t.Fatal("a child of a run whose conversation is gone wrote project memory")
	}
	w.exec(fmt.Sprintf(`CREATE SCHEMA p_%[1]d; CREATE TABLE p_%[1]d.chat_conversations (id serial PRIMARY KEY, uuid uuid NOT NULL)`, project))
	w.exec(fmt.Sprintf(`INSERT INTO p_%d.chat_conversations (uuid) VALUES ($1::uuid)`, project), conversation)
	if facts(MemoryIntentProjectWrite).ProjectMemoryWriteRefused {
		t.Fatal("precondition: an untainted parent's child may write project memory")
	}

	// The parent's conversation is tainted (the parent read personal memory):
	// the child's project-memory write and its remove_index are refused.
	w.exec(`INSERT INTO elitea_runtime.memory_conversation_taints (project_id, conversation_uuid) VALUES ($1::int, $2::uuid)`, project, conversation)
	if !facts(MemoryIntentProjectWrite).ProjectMemoryWriteRefused {
		t.Fatal("a Code child of a tainted parent wrote project memory")
	}
	if !facts(MemoryIntentSharedIndexWrite).ProjectMemoryWriteRefused {
		t.Fatal("a Code child of a tainted parent wrote a shared index")
	}
	if tainted, err := store.ExecutionConversationTainted(ctx, "child", 1); err != nil || !tainted {
		t.Fatalf("a Code child of a tainted parent may remove an index: %v %v", tainted, err)
	}
}

// A Code node's toolkit_call names its toolkit itself (Resource["id"]): the
// child reaches a memory toolkit only if the parent run was given it, so code
// cannot open a memory store of a toolkit the agent never attached.
func TestPostgresCodeToolkitChildReachesOnlyItsParentsMemoryToolkits(t *testing.T) {
	w := newEffectsWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project := w.project()
	w.job("parent", project, "RUNNING")
	w.exec(`UPDATE elitea_runtime.execution_jobs SET memory_toolkit_ids = '{5}' WHERE execution_id = 'parent'`)
	given := func(toolkit int64, parent bool) []int64 {
		t.Helper()
		admission := toolkitcalltoolapp.Admission{
			Binding: executiondomain.ToolkitCallToolBinding{ToolkitType: "memory", ToolkitID: toolkit},
		}
		if parent {
			admission.CodeParent = &codeplatformdomain.ParentEffect{}
			admission.CodeParent.Admission.Job.ExecutionID = "parent"
			admission.CodeParent.Admission.Job.OriginalGeneration = 1
		}
		var ids []int64
		if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
			var err error
			ids, err = callToolChildMemoryToolkits(ctx, tx, admission)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := given(5, true); !slices.Equal(got, []int64{5}) {
		t.Fatalf("the parent's own memory toolkit = %v, want [5]", got)
	}
	if got := given(6, true); got == nil || len(got) != 0 {
		t.Fatalf("a memory toolkit the parent was not given = %v, want none", got)
	}
	// A direct call (no parent: the UI's tool test) keeps its resolved toolkit.
	if got := given(6, false); !slices.Equal(got, []int64{6}) {
		t.Fatalf("a direct call = %v, want [6]", got)
	}
}
