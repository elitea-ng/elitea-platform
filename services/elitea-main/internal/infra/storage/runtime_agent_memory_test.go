package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
)

// fakeAgentMemorySink records writes and answers with a fixed run.
type fakeAgentMemorySink struct {
	run        AgentMemoryRun
	writes     []AgentMemoryWrite
	active     []agentmemory.Candidate
	insertErr  error
	resolveErr error
	forgotten  []int64
	lastTarget AgentMemoryTarget
}

func (f *fakeAgentMemorySink) ResolveAgentMemoryRun(_ context.Context, _ int64, _ string, _ uint64, target AgentMemoryTarget) (AgentMemoryRun, error) {
	f.lastTarget = target
	if f.resolveErr != nil {
		return AgentMemoryRun{}, f.resolveErr
	}
	return f.run, nil
}

func (f *fakeAgentMemorySink) InsertAgentMemory(_ context.Context, _ AgentMemoryRun, write AgentMemoryWrite) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.writes = append(f.writes, write)
	return int64(len(f.writes)), nil
}

func (f *fakeAgentMemorySink) ActiveAgentMemory(context.Context, int64, int64) ([]agentmemory.Candidate, error) {
	return f.active, nil
}

func (f *fakeAgentMemorySink) ForgetAgentMemory(_ context.Context, _ AgentMemoryRun, entryID int64) (bool, error) {
	f.forgotten = append(f.forgotten, entryID)
	return true, nil
}

func agentMemoryService(t *testing.T, sink *fakeAgentMemorySink, conversation string) *RuntimeAgentMemoryService {
	t.Helper()
	authorizer := agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{ResourceProjectID: 7, ActorID: "3", ConversationID: conversation}, nil
	})
	service, err := NewRuntimeAgentMemoryService(authorizer, sink)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func attendedRun() AgentMemoryRun {
	return AgentMemoryRun{
		ProjectID: 7, ApplicationID: 11, ActorID: 3, ConversationUUID: "c-1",
		Settings: agentmemory.DefaultNew, Attended: true, Present: map[int64]string{3: "Dana"},
	}
}

func TestRuntimeAgentMemoryRememberDecidesTheStatus(t *testing.T) {
	ctx := context.Background()
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}
	cases := []struct {
		name string
		run  func(*AgentMemoryRun)
		req  RuntimeAgentMemoryRememberRequest
		want agentmemory.Status
	}{
		{"attended", func(*AgentMemoryRun) {}, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, agentmemory.StatusActive},
		{"after tool output", func(*AgentMemoryRun) {}, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm", FromToolOutput: true}, agentmemory.StatusPending},
		{"unattended", func(run *AgentMemoryRun) { run.Attended = false }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, agentmemory.StatusPending},
		{"personal project unattended", func(run *AgentMemoryRun) { run.Attended = false; run.PersonalProject = true }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm", FromToolOutput: true}, agentmemory.StatusActive},
	}
	for _, c := range cases {
		run := attendedRun()
		c.run(&run)
		sink := &fakeAgentMemorySink{run: run}
		got, err := agentMemoryService(t, sink, "c-1").Remember(ctx, claim, c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Status != string(c.want) || len(sink.writes) != 1 || sink.writes[0].Status != c.want {
			t.Fatalf("%s: got %+v %+v", c.name, got, sink.writes)
		}
		if got.ProjectID != 7 || got.SchemaVersion != RuntimeAgentMemoryRememberSchemaVersion {
			t.Fatalf("%s: envelope %+v", c.name, got)
		}
	}
}

func TestRuntimeAgentMemoryRememberRefusals(t *testing.T) {
	ctx := context.Background()
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}
	cases := []struct {
		name string
		run  func(*AgentMemoryRun)
		req  RuntimeAgentMemoryRememberRequest
		err  error
	}{
		{"secret", func(*AgentMemoryRun) {}, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "api_key = 9f8e7d6c5b4a"}, ErrContentRejected},
		{"cursor is not the agent's", func(*AgentMemoryRun) {}, RuntimeAgentMemoryRememberRequest{Kind: "cursor", Content: "x"}, ErrContentRejected},
		{"taint without a subject", func(run *AgentMemoryRun) { run.Tainted = true }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, ErrContentRejected},
		{"about an absent person", func(run *AgentMemoryRun) { run.Present = nil }, RuntimeAgentMemoryRememberRequest{Kind: "preference", Content: "bullets", AboutUser: true}, ErrContentRejected},
		{"writes off", func(run *AgentMemoryRun) { run.Settings.Write = agentmemory.WriteOff }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, ErrContentRejected},
		{"memory disabled", func(run *AgentMemoryRun) { run.Settings = agentmemory.Off }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, ErrContentRejected},
		{"no agent", func(run *AgentMemoryRun) { run.ApplicationID = 0 }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, ErrContentRejected},
		{"another project", func(run *AgentMemoryRun) { run.ProjectID = 8 }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, ErrContentUnauthorized},
		{"another conversation", func(run *AgentMemoryRun) { run.ConversationUUID = "c-2" }, RuntimeAgentMemoryRememberRequest{Kind: "fact", Content: "uses pnpm"}, ErrContentUnauthorized},
	}
	for _, c := range cases {
		run := attendedRun()
		c.run(&run)
		sink := &fakeAgentMemorySink{run: run}
		if _, err := agentMemoryService(t, sink, "c-1").Remember(ctx, claim, c.req); !errors.Is(err, c.err) {
			t.Fatalf("%s: want %v, got %v", c.name, c.err, err)
		}
		if len(sink.writes) != 0 {
			t.Fatalf("%s: wrote %+v", c.name, sink.writes)
		}
	}
	// Tainted, but about the asking person: allowed.
	run := attendedRun()
	run.Tainted = true
	sink := &fakeAgentMemorySink{run: run}
	if _, err := agentMemoryService(t, sink, "c-1").Remember(ctx, claim, RuntimeAgentMemoryRememberRequest{Kind: "preference", Content: "bullets", AboutUser: true}); err != nil || sink.writes[0].SubjectUserID != 3 {
		t.Fatalf("tainted write about the person: %v %+v", err, sink.writes)
	}
	// An opted-out subject is a refusal, not an outage.
	sink = &fakeAgentMemorySink{run: attendedRun(), insertErr: ErrAgentMemorySubjectOptedOut}
	if _, err := agentMemoryService(t, sink, "c-1").Remember(ctx, claim, RuntimeAgentMemoryRememberRequest{Kind: "preference", Content: "bullets", AboutUser: true}); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("opted out: %v", err)
	}
}

func TestRuntimeAgentMemoryRecallHonoursPresence(t *testing.T) {
	now := time.Now()
	sink := &fakeAgentMemorySink{run: attendedRun(), active: []agentmemory.Candidate{
		{ID: 1, Kind: agentmemory.KindFact, Content: "uses pnpm", UpdatedAt: now},
		{ID: 2, Kind: agentmemory.KindPreference, Content: "Dana wants bullets", SubjectUserID: 3, UpdatedAt: now},
		{ID: 3, Kind: agentmemory.KindPreference, Content: "Lee wants prose", SubjectUserID: 9, UpdatedAt: now},
	}}
	got, err := agentMemoryService(t, sink, "c-1").Recall(context.Background(), ContentClaim{ExecutionID: "e-1"}, RuntimeAgentMemoryRecallRequest{Query: "wants"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, entry := range got.Entries {
		ids[entry.EntryID] = entry.AboutUser
	}
	if _, absent := ids["3"]; absent || ids["2"] != "Dana" || len(ids) != 2 {
		t.Fatalf("recall: %+v", got.Entries)
	}
	if _, err := agentMemoryService(t, sink, "c-1").Recall(context.Background(), ContentClaim{}, RuntimeAgentMemoryRecallRequest{Limit: agentmemory.MaxRecallEntries + 1}); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("over-limit recall: %v", err)
	}
}

func TestRuntimeAgentMemoryForgetNeedsWrites(t *testing.T) {
	run := attendedRun()
	run.Settings.Write = agentmemory.WriteOff
	sink := &fakeAgentMemorySink{run: run}
	if _, err := agentMemoryService(t, sink, "c-1").Forget(context.Background(), ContentClaim{}, RuntimeAgentMemoryForgetRequest{EntryID: "5"}); !errors.Is(err, ErrContentRejected) || len(sink.forgotten) != 0 {
		t.Fatalf("forget with writes off: %v %v", err, sink.forgotten)
	}
	sink = &fakeAgentMemorySink{run: attendedRun()}
	got, err := agentMemoryService(t, sink, "c-1").Forget(context.Background(), ContentClaim{}, RuntimeAgentMemoryForgetRequest{EntryID: "5"})
	if err != nil || !got.Forgotten || sink.forgotten[0] != 5 {
		t.Fatalf("forget: %+v %v", got, err)
	}
	if _, err := agentMemoryService(t, sink, "c-1").Forget(context.Background(), ContentClaim{}, RuntimeAgentMemoryForgetRequest{EntryID: "x"}); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestRuntimeAgentMemoryNestedTargetValidationAndSkew(t *testing.T) {
	ctx := context.Background()
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}

	badTargets := []struct {
		name string
		app  int64
		ver  int64
	}{
		{"only app id", 42, 0},
		{"only version id", 0, 9},
		{"negative app id", -1, 9},
		{"negative version id", 42, -1},
		{"overflow app id", math.MaxInt32 + 1, 9},
		{"overflow version id", 42, math.MaxInt32 + 1},
	}
	for _, tc := range badTargets {
		sink := &fakeAgentMemorySink{run: attendedRun()}
		svc := agentMemoryService(t, sink, "c-1")
		if _, err := svc.Remember(ctx, claim, RuntimeAgentMemoryRememberRequest{
			Kind: "fact", Content: "note", TargetApplicationID: tc.app, TargetVersionID: tc.ver,
		}); !errors.Is(err, ErrContentRejected) {
			t.Fatalf("remember %s: want ErrContentRejected, got %v", tc.name, err)
		}
		if _, err := svc.Recall(ctx, claim, RuntimeAgentMemoryRecallRequest{
			Query: "q", TargetApplicationID: tc.app, TargetVersionID: tc.ver,
		}); !errors.Is(err, ErrContentRejected) {
			t.Fatalf("recall %s: want ErrContentRejected, got %v", tc.name, err)
		}
		if _, err := svc.Forget(ctx, claim, RuntimeAgentMemoryForgetRequest{
			EntryID: "5", TargetApplicationID: tc.app, TargetVersionID: tc.ver,
		}); !errors.Is(err, ErrContentRejected) {
			t.Fatalf("forget %s: want ErrContentRejected, got %v", tc.name, err)
		}
	}

	// Valid nested target passes to sink and fails closed if sink returns another app.
	mismatched := attendedRun()
	mismatched.ApplicationID = 11 // root, not requested 42
	sink := &fakeAgentMemorySink{run: mismatched}
	if _, err := agentMemoryService(t, sink, "c-1").Recall(ctx, claim, RuntimeAgentMemoryRecallRequest{
		Query: "q", TargetApplicationID: 42, TargetVersionID: 9,
	}); !errors.Is(err, ErrContentUnauthorized) {
		t.Fatalf("mismatched sink app id: want ErrContentUnauthorized, got %v", err)
	}
	if sink.lastTarget != (AgentMemoryTarget{ApplicationID: 42, VersionID: 9}) {
		t.Fatalf("lastTarget = %+v, want {42 9}", sink.lastTarget)
	}

	// Matching nested target succeeds; unattended nested write lands pending (D3).
	nestedRun := attendedRun()
	nestedRun.ApplicationID = 42
	nestedRun.DelegatedFromApplicationID = 11
	nestedRun.Attended = false
	sink = &fakeAgentMemorySink{run: nestedRun}
	got, err := agentMemoryService(t, sink, "c-1").Remember(ctx, claim, RuntimeAgentMemoryRememberRequest{
		Kind: "fact", Content: "nested note", TargetApplicationID: 42, TargetVersionID: 9,
	})
	if err != nil || got.Status != string(agentmemory.StatusPending) {
		t.Fatalf("nested remember: got %+v, err %v", got, err)
	}

	// Strict JSON decoding (version-skew contract with content_server.go DisallowUnknownFields):
	// old worker omits target fields (valid), new worker sends target fields (valid),
	// unknown extra field fails closed.
	for _, body := range []string{
		`{"kind":"fact","content":"root note","about_user":false,"from_tool_output":false}`,
		`{"kind":"fact","content":"nested note","about_user":false,"from_tool_output":false,"target_application_id":42,"target_version_id":9}`,
	} {
		var req RuntimeAgentMemoryRememberRequest
		dec := json.NewDecoder(bytes.NewReader([]byte(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("strict decode %s: %v", body, err)
		}
	}
	var unknown RuntimeAgentMemoryRememberRequest
	dec := json.NewDecoder(bytes.NewReader([]byte(`{"kind":"fact","content":"x","unexpected":1}`)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&unknown); err == nil {
		t.Fatal("strict decode accepted unknown field")
	}
}
