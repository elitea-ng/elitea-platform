package localturn

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// hostReportCommit is a commit whose report has a sandboxed command only, as
// every client before contract 1.66 sends it.
func hostReportCommit() CommitRequest {
	code := 0
	return CommitRequest{
		ProjectID: 1, ActorUserID: 2, ExecutionID: strings.Repeat("a", 32), Credential: Credential{TokenID: "70"},
		UserMessage: "hi", AssistantMessage: "hello",
		Report: LocalWorkReport{SandboxMode: "workspace-write", Enforcement: "full",
			Commands: []CommandRecord{{Command: "ls", ExitCode: &code}}, Paths: []string{"a.txt"}},
	}
}

// TestCommitStoresHostCommands pins ADR-0061 decision 5i (issue 3060): a
// report with host commands is stored with its count and the folder's trust,
// stamps `local_work.ran_outside_sandbox`, and keeps `execution: host` on its
// commands (any other execution value is dropped).
func TestCommitStoresHostCommands(t *testing.T) {
	store := &commitFakeStore{}
	service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := hostReportCommit()
	request.Report.HostCommands = 2
	request.Report.Trusted = true
	request.Report.Commands = []CommandRecord{
		{Command: "kubectl get pods", Execution: ExecutionHost},
		{Command: "ls", Execution: "sandbox-escape"},
	}
	if _, err := service.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		LocalWork struct {
			HostCommands      int  `json:"host_commands"`
			Trusted           bool `json:"trusted"`
			RanOutsideSandbox bool `json:"ran_outside_sandbox"`
			Commands          []struct {
				Command   string `json:"command"`
				Execution string `json:"execution"`
			} `json:"commands"`
		} `json:"local_work"`
	}
	if err := json.Unmarshal(store.last.ResponseMeta, &meta); err != nil {
		t.Fatal(err)
	}
	got := meta.LocalWork
	if got.HostCommands != 2 || !got.Trusted || !got.RanOutsideSandbox {
		t.Fatalf("local_work = %+v, want host_commands 2, trusted and ran_outside_sandbox", got)
	}
	if len(got.Commands) != 2 || got.Commands[0].Execution != ExecutionHost || got.Commands[1].Execution != "" {
		t.Fatalf("commands = %+v, want execution host on the first only", got.Commands)
	}
}

// TestBoundReportWithoutHostFieldsIsUnchanged: an older report carries none
// of the new keys in the stored meta, and a negative count is not stored.
func TestBoundReportWithoutHostFieldsIsUnchanged(t *testing.T) {
	encoded, err := json.Marshal(boundReport(hostReportCommit().Report))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"host_commands", "trusted", "ran_outside_sandbox", "execution"} {
		if strings.Contains(string(encoded), `"`+key+`"`) {
			t.Fatalf("an older report must not carry %q: %s", key, encoded)
		}
	}
	if got := boundReport(LocalWorkReport{HostCommands: -3}); got.HostCommands != 0 || got.RanOutsideSandbox {
		t.Fatalf("a negative count = %+v, want none", got)
	}
}

// TestCommitDigestWithoutHostFieldsIsPinned: the digest of a report without
// the 1.66 fields is the one the code before them computed (pinned from that
// code), so a replayed commit of an older client still matches.
func TestCommitDigestWithoutHostFieldsIsPinned(t *testing.T) {
	request := hostReportCommit()
	digest, err := commitDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	const pinned = "992b8776ee2a61ec357c8f7bbdae15022605c463d7c8c64fa393f94395a3617f"
	if got := hex.EncodeToString(digest[:]); got != pinned {
		t.Fatalf("digest = %s, want the pre-1.66 %s", got, pinned)
	}
	request.Report.HostCommands = 2
	if moved, _ := commitDigest(request); moved == digest {
		t.Fatal("the digest must see host_commands")
	}
}
