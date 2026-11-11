package control

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type ephemeralAgentsStub struct {
	calls  *[]string
	fences []runtimedomain.Fence
	agents [][]EphemeralAgent
	err    error
}

func (s *ephemeralAgentsStub) RecordEphemeralAgents(_ context.Context, fence runtimedomain.Fence, agents []EphemeralAgent) error {
	*s.calls = append(*s.calls, "record-ephemeral-agents")
	if s.err != nil {
		return s.err
	}
	s.fences = append(s.fences, fence)
	s.agents = append(s.agents, agents)
	return nil
}

var ephemeralTestDigest = strings.Repeat("ab", 32)

func ephemeralAgentWire() *runtimev1.EphemeralAgentV1 {
	return &runtimev1.EphemeralAgentV1{
		AgentId: "a_0011aabb", ParentCallId: "call-1", Ordinal: 1, Label: "Owl", DisplayName: "Owl",
		Avatar: "default:owl", ProfileDigest: ephemeralTestDigest, BriefDigest: ephemeralTestDigest,
		BriefRefsJson: `{"message_ids":["m-1"],"attachments":[],"memory_entries":[],"facts":1}`,
		Status:        "completed", Steps: 3, InputTokens: 100, OutputTokens: 20,
		StartedAtUnixMillis: 1_800_000_000_000, FinishedAtUnixMillis: 1_800_000_001_000,
	}
}

func ephemeralAgentsRequestFor(fence runtimedomain.Fence, agents ...*runtimev1.EphemeralAgentV1) *runtimev1.ReportEphemeralAgentsRequestV1 {
	return &runtimev1.ReportEphemeralAgentsRequestV1{Identity: identityForFence(fence), Fence: fenceProto(fence), Agents: agents}
}

// The live claim is verified as ReportEgressSummary verifies it, and the
// spawns reach the store under the verified fence.
func TestReportEphemeralAgentsVerifiesTheClaimAndStores(t *testing.T) {
	calls := []string{}
	lease := validLease()
	store := &ephemeralAgentsStub{calls: &calls}
	server := newSteeringTestServer(t, &calls, lease).WithEphemeralAgents(store)
	response, err := server.ReportEphemeralAgents(context.Background(), ephemeralAgentsRequestFor(lease.Fence, ephemeralAgentWire()))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-fence", "record-ephemeral-agents"}) {
		t.Fatalf("calls=%v", calls)
	}
	if len(store.fences) != 1 || store.fences[0] != lease.Fence {
		t.Fatalf("fences=%+v", store.fences)
	}
	got := store.agents[0]
	if len(got) != 1 || got[0].AgentID != "a_0011aabb" || got[0].Ordinal != 1 || got[0].Status != "completed" ||
		!got[0].StartedAt.Equal(time.UnixMilli(1_800_000_000_000)) || !got[0].FinishedAt.Equal(time.UnixMilli(1_800_000_001_000)) ||
		string(got[0].BriefRefs) == "" {
		t.Fatalf("agent=%+v", got)
	}

	// A running child without a brief: no finish time, no references.
	running := ephemeralAgentWire()
	running.Status, running.FinishedAtUnixMillis, running.BriefDigest, running.BriefRefsJson = "running", 0, "", ""
	response, err = server.ReportEphemeralAgents(context.Background(), ephemeralAgentsRequestFor(lease.Fence, running))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if last := store.agents[1][0]; !last.FinishedAt.IsZero() || last.BriefRefs != nil {
		t.Fatalf("running=%+v", last)
	}
}

func grandchildOf(agentID, parentID string) *runtimev1.EphemeralAgentV1 {
	agent := ephemeralAgentWire()
	agent.AgentId, agent.Depth, agent.ParentAgentId = agentID, 2, parentID
	return agent
}

// A depth-2 run reports two levels (#1872): a child (depth 0 from an earlier
// worker reads as 1) and the grandchild under it.
func TestReportEphemeralAgentsCarriesDepthAndParent(t *testing.T) {
	calls := []string{}
	lease := validLease()
	store := &ephemeralAgentsStub{calls: &calls}
	server := newSteeringTestServer(t, &calls, lease).WithEphemeralAgents(store)
	child := ephemeralAgentWire()
	child.AgentId = "a_00000001"
	grandchild := grandchildOf("a_00000002", "a_00000001")
	grandchild.Ordinal = 2
	response, err := server.ReportEphemeralAgents(context.Background(), ephemeralAgentsRequestFor(lease.Fence, child, grandchild))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	got := store.agents[0]
	if len(got) != 2 || got[0].Depth != 1 || got[0].ParentAgentID != "" ||
		got[1].Depth != 2 || got[1].ParentAgentID != "a_00000001" {
		t.Fatalf("agents=%+v", got)
	}
}

// A grandchild whose parent the report does not carry (the worker lost the
// run's ledger) is dropped alone; the rest of the report is stored.
func TestReportEphemeralAgentsDropsOnlyAnOrphanGrandchild(t *testing.T) {
	calls := []string{}
	lease := validLease()
	store := &ephemeralAgentsStub{calls: &calls}
	server := newSteeringTestServer(t, &calls, lease).WithEphemeralAgents(store)
	child := ephemeralAgentWire()
	child.AgentId = "a_00000001"
	orphan := grandchildOf("a_00000002", "a_00000009")
	orphan.Ordinal = 2
	response, err := server.ReportEphemeralAgents(context.Background(), ephemeralAgentsRequestFor(lease.Fence, child, orphan))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if len(store.agents) != 1 || len(store.agents[0]) != 1 || store.agents[0][0].AgentID != "a_00000001" {
		t.Fatalf("agents=%+v", store.agents)
	}

	// A report of nothing but orphans stores nothing and is not refused.
	store.agents = nil
	response, err = server.ReportEphemeralAgents(context.Background(), ephemeralAgentsRequestFor(lease.Fence, orphan))
	if err != nil || response.GetRejection() != nil || len(store.agents) != 0 {
		t.Fatalf("err=%v rejection=%v agents=%+v", err, response.GetRejection(), store.agents)
	}
}

func TestReportEphemeralAgentsRefusals(t *testing.T) {
	lease := validLease()
	stale := lease.Fence
	stale.Token[0] ^= 0xff
	mutate := func(change func(*runtimev1.EphemeralAgentV1)) *runtimev1.ReportEphemeralAgentsRequestV1 {
		agent := ephemeralAgentWire()
		change(agent)
		return ephemeralAgentsRequestFor(lease.Fence, agent)
	}
	tooMany := ephemeralAgentsRequestFor(lease.Fence)
	for i := range MaxEphemeralAgents + 1 {
		agent := ephemeralAgentWire()
		agent.AgentId = fmt.Sprintf("a_%08x", i)
		tooMany.Agents = append(tooMany.Agents, agent)
	}
	protocol := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION
	for _, test := range []struct {
		name      string
		store     *ephemeralAgentsStub
		request   *runtimev1.ReportEphemeralAgentsRequestV1
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
	}{
		{name: "not composed", request: ephemeralAgentsRequestFor(lease.Fence, ephemeralAgentWire()), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY},
		{name: "malformed", store: &ephemeralAgentsStub{}, request: &runtimev1.ReportEphemeralAgentsRequestV1{}, code: protocol},
		{name: "too many", store: &ephemeralAgentsStub{}, request: tooMany, code: protocol},
		{name: "duplicate agent", store: &ephemeralAgentsStub{}, request: ephemeralAgentsRequestFor(lease.Fence, ephemeralAgentWire(), ephemeralAgentWire()), code: protocol},
		{name: "agent id", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.AgentId = "child-1" }), code: protocol},
		{name: "profile digest", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.ProfileDigest = "" }), code: protocol},
		{name: "digest without refs", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.BriefRefsJson = "" }), code: protocol},
		{name: "refs not an object", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.BriefRefsJson = "[1]" }), code: protocol},
		{name: "status", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.Status = "done" }), code: protocol},
		{name: "label", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.Label = strings.Repeat("x", 61) }), code: protocol},
		{name: "control character", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.DisplayName = "x\x00y" }), code: protocol},
		{name: "call id control character", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.ParentCallId = "c\n1" }), code: protocol},
		{name: "steps", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.Steps = 1 << 31 }), code: protocol},
		{name: "error code", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.ErrorCode = "Bad Code" }), code: protocol},
		{name: "finished before start", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.FinishedAtUnixMillis = 1 }), code: protocol},
		{name: "ordinal", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.Ordinal = 0 }), code: protocol},
		{name: "depth past two", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.Depth = 3 }), code: protocol},
		{name: "parent at depth one", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.ParentAgentId = "a_00000009" }), code: protocol},
		{name: "depth two without parent", store: &ephemeralAgentsStub{}, request: mutate(func(a *runtimev1.EphemeralAgentV1) { a.Depth = 2 }), code: protocol},
		{name: "parent is a grandchild", store: &ephemeralAgentsStub{}, request: ephemeralAgentsRequestFor(lease.Fence, grandchildOf("a_00000001", "a_00000002"), grandchildOf("a_00000002", "a_00000001")), code: protocol},
		{name: "stale fence", store: &ephemeralAgentsStub{}, request: ephemeralAgentsRequestFor(stale, ephemeralAgentWire()), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "store unavailable", store: &ephemeralAgentsStub{err: errors.New("database down")}, request: ephemeralAgentsRequestFor(lease.Fence, ephemeralAgentWire()), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			server := newSteeringTestServer(t, &calls, lease)
			if test.store != nil {
				test.store.calls = &calls
				server.WithEphemeralAgents(test.store)
			}
			response, err := server.ReportEphemeralAgents(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection == nil || rejection.GetCode() != test.code || rejection.GetRetryable() != test.retryable {
				t.Fatalf("rejection=%v", rejection)
			}
			if test.store != nil && test.code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE && len(test.store.agents) != 0 {
				t.Fatal("a refused report was stored")
			}
			if strings.Contains(rejection.GetSafeMessage(), "database") {
				t.Fatalf("an internal error leaked: %q", rejection.GetSafeMessage())
			}
		})
	}
}
