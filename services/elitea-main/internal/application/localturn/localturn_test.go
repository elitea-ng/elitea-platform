package localturn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestBoundReportCutsAndCounts(t *testing.T) {
	report := LocalWorkReport{Commands: make([]CommandRecord, 0, 60), Paths: make([]string, 0, 210)}
	for i := 0; i < 60; i++ {
		report.Commands = append(report.Commands, CommandRecord{Command: strings.Repeat("x", 400) + "\nnext"})
	}
	for i := 0; i < 210; i++ {
		report.Paths = append(report.Paths, "src/\x00file.rs")
	}
	got := boundReport(report)
	if len(got.Commands) != MaxReportedCommands || got.CommandsTotal != 60 {
		t.Fatalf("commands = %d (total %d)", len(got.Commands), got.CommandsTotal)
	}
	if len(got.Commands[0].Command) > MaxReportedCommandBytes || strings.ContainsAny(got.Commands[0].Command, "\n") {
		t.Fatalf("command not bounded/cleaned: %q", got.Commands[0].Command)
	}
	if len(got.Paths) != MaxReportedPaths || got.PathsTotal != 210 || got.Paths[0] != "src/ file.rs" {
		t.Fatalf("paths = %d (total %d) first %q", len(got.Paths), got.PathsTotal, got.Paths[0])
	}
	action := commitAuditAction(got)
	if len([]rune(action)) > 512 || !strings.Contains(action, "60 commands") || !strings.Contains(action, "210 paths") {
		t.Fatalf("audit action = %q", action)
	}
}

func TestValidCommitRules(t *testing.T) {
	base := CommitRequest{
		ProjectID: 1, ActorUserID: 2, ExecutionID: strings.Repeat("a", 32), Credential: Credential{TokenID: "70"},
		UserMessage: "q", AssistantMessage: "a",
	}
	if !validCommit(base) {
		t.Fatal("a minimal commit is valid")
	}
	for name, mutate := range map[string]func(*CommitRequest){
		"upper-case id":      func(r *CommitRequest) { r.ExecutionID = strings.Repeat("A", 32) },
		"no credential":      func(r *CommitRequest) { r.Credential = Credential{} },
		"empty question":     func(r *CommitRequest) { r.UserMessage = "" },
		"NUL in answer":      func(r *CommitRequest) { r.AssistantMessage = "a\x00" },
		"error without flag": func(r *CommitRequest) { r.AssistantError = "boom" },
		"unknown sandbox":    func(r *CommitRequest) { r.Report.SandboxMode = "root" },
		"unknown enforcement": func(r *CommitRequest) {
			r.Report.Enforcement = "maybe"
		},
		"HITL without id": func(r *CommitRequest) {
			r.HITLExchanges = []HITLExchange{{Kind: "k", Decision: "approve"}}
		},
	} {
		request := base
		mutate(&request)
		if validCommit(request) {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A retry that sends an absent list where the first attempt sent an empty one
// (or the reverse) is the same body, not an ErrAlreadyCommitted conflict.
func TestCommitDigestTreatsAbsentAndEmptyAsTheSame(t *testing.T) {
	absent := CommitRequest{ExecutionID: "x", UserMessage: "q", AssistantMessage: "a"}
	empty := CommitRequest{ExecutionID: "x", UserMessage: "q", AssistantMessage: "a",
		ToolCalls: json.RawMessage(` {} `), ThinkingSteps: []json.RawMessage{}, HITLExchanges: []HITLExchange{},
		Report: LocalWorkReport{Commands: []CommandRecord{}, Paths: []string{}}}
	null := CommitRequest{ExecutionID: "x", UserMessage: "q", AssistantMessage: "a", ToolCalls: json.RawMessage(`null`)}
	da, err := commitDigest(absent)
	if err != nil {
		t.Fatal(err)
	}
	de, _ := commitDigest(empty)
	dn, _ := commitDigest(null)
	if da != de || da != dn {
		t.Fatal("absent, empty and null parts must digest the same")
	}
	withPath := empty
	withPath.Report.Paths = []string{"a"}
	if dp, _ := commitDigest(withPath); dp == da {
		t.Fatal("the digest must still see content")
	}
}

func TestCommitDigestIgnoresWhitespaceOnly(t *testing.T) {
	a := CommitRequest{ExecutionID: "x", ToolCalls: json.RawMessage(`{"r": {"a": 1}}`)}
	b := CommitRequest{ExecutionID: "x", ToolCalls: json.RawMessage("{\"r\":{\"a\":1}}\n")}
	c := CommitRequest{ExecutionID: "x", ToolCalls: json.RawMessage(`{"r":{"a":2}}`)}
	da, _ := commitDigest(a)
	db, _ := commitDigest(b)
	dc, _ := commitDigest(c)
	if da != db || da == dc {
		t.Fatal("digest must ignore whitespace and see content")
	}
}

func TestResponseMessageIDIsStableAndOwnNamespace(t *testing.T) {
	const question = "11111111-2222-4333-8444-555555555555"
	first, second := ResponseMessageID(question), ResponseMessageID(question)
	if first != second || !ValidUUID(first) {
		t.Fatal("response id must be a stable lowercase uuid")
	}
	if first == ResponseMessageID("11111111-2222-4333-8444-555555555556") {
		t.Fatal("response id must depend on the question")
	}
}

func TestCleanReportedReplacesControlAndFormatRunes(t *testing.T) {
	cases := map[string]string{
		"a\u202eb":         "a b", // bidi override
		"a\u2066b\u2069c":  "a b c",
		"a\u200eb\u200fc":  "a b c",
		"a\u200bb":         "a b", // zero width space (Cf)
		"a\u0085b":         "a b", // C1 control
		"a\x7fb\tc":        "a b c",
		"  trimmed\u202d ": "trimmed",
		"bad\xffutf8":      "badutf8",
	}
	for in, want := range cases {
		if got := cleanReported(in, 100); got != want {
			t.Errorf("cleanReported(%q) = %q, want %q", in, got, want)
		}
	}
	// Multi-byte runes are never split by the byte limit.
	if got := cleanReported("ééé", 5); got != "éé" {
		t.Errorf("limit cut = %q", got)
	}
}

type commitFakeStore struct{ commits int }

func (s *commitFakeStore) StartLocalTurn(context.Context, StartRecord) (StartedTurn, error) {
	return StartedTurn{}, nil
}

func (s *commitFakeStore) CommitLocalTurn(context.Context, CommitRecord) (CommittedTurn, error) {
	s.commits++
	return CommittedTurn{}, nil
}

type commitFakePolicy struct {
	allowed bool
	err     error
}

func (p commitFakePolicy) Policy(context.Context) (platformconfig.NativeClientPolicy, error) {
	policy := platformconfig.DefaultNativeClientPolicy()
	policy.LocalWork.Allowed = p.allowed
	return policy, p.err
}

type commitFakeMemories struct{}

func (commitFakeMemories) ResolveCurrentMemoryRecall(context.Context, int64, int64, string) (agentexecutionapp.CurrentMemoryRecall, error) {
	return agentexecutionapp.CurrentMemoryRecall{}, nil
}

func (commitFakeMemories) RecordCurrentMemoryUsage(context.Context, int64, string, int) error {
	return nil
}

type commitFakeAudit struct{}

func (commitFakeAudit) Record(context.Context, audit.Event) {}

func TestCommitHonoursTheLocalWorkPolicy(t *testing.T) {
	commit := CommitRequest{
		ProjectID: 1, ActorUserID: 2, ExecutionID: strings.Repeat("a", 32), Credential: Credential{TokenID: "70"},
		UserMessage: "hi", AssistantMessage: "hello",
	}
	for name, test := range map[string]struct {
		policy commitFakePolicy
		want   error
		writes int
	}{
		"allowed":    {commitFakePolicy{allowed: true}, nil, 1},
		"disabled":   {commitFakePolicy{allowed: false}, ErrLocalWorkDisabled, 0},
		"unreadable": {commitFakePolicy{allowed: true, err: errors.New("db down")}, ErrUnavailable, 0},
	} {
		t.Run(name, func(t *testing.T) {
			store := &commitFakeStore{}
			service, err := NewService(store, test.policy, commitFakeMemories{}, commitFakeAudit{},
				func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Commit(context.Background(), commit); !errors.Is(err, test.want) {
				t.Fatalf("Commit = %v, want %v", err, test.want)
			}
			if store.commits != test.writes {
				t.Fatalf("store commits = %d, want %d", store.commits, test.writes)
			}
		})
	}
}
