package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
)

// fakeAgentMemorySink records writes and answers with a fixed run.
type fakeAgentMemorySink struct {
	run       AgentMemoryRun
	writes    []AgentMemoryWrite
	active    []agentmemory.Candidate
	insertErr error
	forgotten []int64
}

func (f *fakeAgentMemorySink) ResolveAgentMemoryRun(context.Context, int64, string, uint64) (AgentMemoryRun, error) {
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
