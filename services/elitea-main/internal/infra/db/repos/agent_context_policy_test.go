package repos

import (
	"crypto/sha256"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func TestPostgresContinuationContextPolicyUsesExactAdmittedInput(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	const conversation = "10000000-0000-4000-8000-000000000071"
	const response = "20000000-0000-4000-8000-000000000071"
	const generation = "30000000-0000-4000-8000-000000000071"
	const other = "40000000-0000-4000-8000-000000000071"
	want := &runtimev1.SummaryModelSnapshotV1{
		LlmSettings:        []byte(`{"model_name":"summary","model_project_id":1,"max_tokens":4000,"openai_compatible":true,"temperature":null}`),
		ModelContextLimits: &runtimev1.ModelContextLimitsV1{ContextWindowTokens: 32000, MaxOutputTokens: 4000},
	}
	admitPostgresAgentExecution(t, pool, conversation, response, generation, func(request *agentexecutionapp.SubmitRequest) {
		request.Identity.TenantID = "1"
		request.Input.ConversationId = proto.String(conversation)
		request.Input.ContextSettings = []byte(`{"enabled":true,"budget_mode":"balanced","preserve_recent_messages":9}`)
		request.Input.SummaryModel = want
	})
	repository := NewCurrentAgentContextPolicyRepository(pool)
	policy, err := repository.ContinuationContextPolicy(t.Context(), 1, 7, conversation, response, generation)
	if err != nil || string(policy.Settings) != `{"enabled":true,"budget_mode":"balanced","preserve_recent_messages":9}` || !proto.Equal(policy.SummaryModel, want) {
		t.Fatalf("frozen policy changed: %+v, %v", policy, err)
	}
	for _, tc := range []struct {
		project, actor                     int64
		conversation, response, generation string
	}{
		{2, 7, conversation, response, generation},
		{1, 8, conversation, response, generation},
		{1, 7, other, response, generation},
		{1, 7, conversation, other, generation},
		{1, 7, conversation, response, other},
	} {
		if _, err := repository.ContinuationContextPolicy(t.Context(), tc.project, tc.actor, tc.conversation, tc.response, tc.generation); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("foreign policy was returned: %+v, %v", tc, err)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE elitea_runtime.input_bundle_entries SET content_digest = $1 WHERE input_bundle_id = 'bundle-agent'`, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ContinuationContextPolicy(t.Context(), 1, 7, conversation, response, generation); err == nil {
		t.Fatal("corrupted input passed digest verification")
	}
}

func TestFrozenContextPolicyDecodeRejectsIdentityAndDigestDrift(t *testing.T) {
	const conversation, generation = "conversation", "generation"
	input := &runtimev1.AgentExecutionInputV1{SchemaRevision: "elitea.runtime.agent-execution-input.v1", ConversationId: proto.String(conversation), ExecutionGeneration: proto.String(generation), ContextSettings: []byte(`{}`)}
	encoded, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	policy, err := decodeFrozenContextPolicy(encoded, digest[:], conversation, generation)
	if err != nil || string(policy.Settings) != `{}` || policy.SummaryModel != nil {
		t.Fatalf("legacy policy changed: %+v, %v", policy, err)
	}
	for _, tc := range []struct {
		conversation, generation string
		digest                   []byte
	}{
		{"other", generation, digest[:]}, {conversation, "other", digest[:]}, {conversation, generation, make([]byte, 32)},
	} {
		if _, err := decodeFrozenContextPolicy(encoded, tc.digest, tc.conversation, tc.generation); err == nil {
			t.Fatalf("invalid stored identity passed: %+v", tc)
		}
	}
}
