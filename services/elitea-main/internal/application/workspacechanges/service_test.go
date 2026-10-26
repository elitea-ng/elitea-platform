package workspacechanges_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

const (
	conversation = "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60"
	handoffID    = "6b1f3f0e-6a52-4d0b-9a55-7f3c2f2f7a10"
	snapshot     = "1111111111111111111111111111111111111111"
	turnOne      = "2222222222222222222222222222222222222222"
	turnTwo      = "3333333333333333333333333333333333333333"
	abandoned    = "4444444444444444444444444444444444444444"
)

var thread = workspacehandoff.Thread{ProjectID: 7, ConversationUUID: conversation}

type memoryRepo struct {
	records map[string]workspacechanges.Record
	diffs   map[string]string
}

func newRepo() *memoryRepo {
	return &memoryRepo{records: map[string]workspacechanges.Record{}, diffs: map[string]string{}}
}

func (m *memoryRepo) Save(_ context.Context, record workspacechanges.Record) error {
	m.records[record.Commit] = record
	for _, diff := range record.Diffs {
		m.diffs[record.Commit+"|"+diff.Scope+"|"+diff.Path] = diff.Text
	}
	return nil
}

func (m *memoryRepo) Turns(_ context.Context, _ workspacehandoff.Thread, handoff string, _ int) ([]workspacechanges.Stored, error) {
	out := []workspacechanges.Stored{}
	for _, record := range m.records {
		if record.HandoffID == handoff {
			out = append(out, workspacechanges.Stored{Commit: record.Commit, Parent: record.Parent, Seq: record.Seq,
				ResponseMessageID: record.ResponseMessageID, CreatedAt: time.Unix(record.Seq, 0), Turn: record.Turn, Cumulative: record.Cumulative})
		}
	}
	return out, nil
}

func (m *memoryRepo) Diff(_ context.Context, _ workspacehandoff.Thread, commit, scope, path string) (string, bool, error) {
	text, ok := m.diffs[commit+"|"+scope+"|"+path]
	return text, ok, nil
}

type handoffs struct{ row workspacehandoff.Handoff }

func (h *handoffs) Read(context.Context, workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error) {
	return h.row, h.row.HandoffID != "", nil
}

type policies struct {
	policy workspacehandoff.Policy
	err    error
}

func (p *policies) EffectivePolicy(context.Context, int64) (workspacehandoff.Policy, error) {
	return p.policy, p.err
}

func sha(value string) *string { return &value }

func count(value uint32) *uint32 { return &value }

func harness(t *testing.T) (*workspacechanges.Service, *memoryRepo, *handoffs, *policies) {
	t.Helper()
	repo := newRepo()
	rows := &handoffs{row: workspacehandoff.Handoff{
		ProjectID: 7, ConversationUUID: conversation, HandoffID: handoffID, HandoffSHA: sha(snapshot),
		Excluded: []string{"config/local.yaml"}, RunningResponseID: "answer-1",
	}}
	policy := &policies{policy: workspacehandoff.Policy{Exclusions: []string{".env", "*.pem"}, PathDeny: []string{"secrets/"}}}
	service, err := workspacechanges.NewService(repo, rows, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, rows, policy
}

func text(scope, path, status string) workspaceruntime.ChangedFile {
	return workspaceruntime.ChangedFile{Scope: scope, Path: path, Status: status, Additions: count(2), Deletions: count(1), Diff: "@@ -1 +1,2 @@\n-a\n+b\n+c\n"}
}

func record(t *testing.T, service *workspacechanges.Service, rows *handoffs, commit, parent string, seq int64, files ...workspaceruntime.ChangedFile) {
	t.Helper()
	rows.row.CloudSHA = sha(commit)
	err := service.Record(context.Background(), rows.row, "exec-"+commit[:4], seq,
		workspaceruntime.TurnChanges{Commit: commit, Parent: parent, Base: snapshot, Files: files})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRecordNeverKeepsAHiddenPathOrItsDiff: a file the platform exclusions,
// the path deny list or the send's excluded list name, and a rename from or
// to one, is counted as hidden; neither its path nor its diff is stored.
func TestRecordNeverKeepsAHiddenPathOrItsDiff(t *testing.T) {
	service, repo, rows, _ := harness(t)
	renamedOut := workspaceruntime.ChangedFile{Scope: workspaceruntime.ChangeScopeTurn, Path: "public/key.txt",
		FromPath: "secrets/key.txt", Status: workspaceruntime.ChangeRenamed}
	record(t, service, rows, turnOne, snapshot, 1,
		text(workspaceruntime.ChangeScopeTurn, "src/main.rs", workspaceruntime.ChangeModified),
		text(workspaceruntime.ChangeScopeTurn, ".env", workspaceruntime.ChangeAdded),
		text(workspaceruntime.ChangeScopeTurn, "deploy/tls/server.PEM", workspaceruntime.ChangeAdded),
		text(workspaceruntime.ChangeScopeTurn, "secrets/a/b.txt", workspaceruntime.ChangeModified),
		text(workspaceruntime.ChangeScopeCumulative, "Config/Local.yaml", workspaceruntime.ChangeModified),
		renamedOut,
	)
	stored := repo.records[turnOne]
	if len(stored.Turn.Files) != 1 || stored.Turn.Files[0].Path != "src/main.rs" || stored.Turn.Hidden != 4 ||
		len(stored.Cumulative.Files) != 0 || stored.Cumulative.Hidden != 1 {
		t.Fatalf("stored = %+v", stored)
	}
	if len(stored.Diffs) != 1 || stored.Diffs[0].Path != "src/main.rs" || stored.ResponseMessageID != "answer-1" || stored.Seq != 1 {
		t.Fatalf("diffs = %+v", stored.Diffs)
	}
}

// TestViewWalksTheLiveChainOnly: the turns are the parents from the cloud tip
// to the snapshot, oldest first; a regenerated answer's earlier attempt (a
// record off the chain) is not listed, and the cumulative set is the tip's.
func TestViewWalksTheLiveChainOnly(t *testing.T) {
	service, _, rows, _ := harness(t)
	record(t, service, rows, turnOne, snapshot, 1, text(workspaceruntime.ChangeScopeTurn, "a.go", workspaceruntime.ChangeAdded))
	record(t, service, rows, abandoned, turnOne, 2, text(workspaceruntime.ChangeScopeTurn, "gone.go", workspaceruntime.ChangeAdded))
	record(t, service, rows, turnTwo, turnOne, 3,
		text(workspaceruntime.ChangeScopeTurn, "b.go", workspaceruntime.ChangeModified),
		text(workspaceruntime.ChangeScopeCumulative, "a.go", workspaceruntime.ChangeAdded),
		text(workspaceruntime.ChangeScopeCumulative, "b.go", workspaceruntime.ChangeModified),
	)
	view, err := service.View(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Complete || len(view.Turns) != 2 || view.Turns[0].Commit != turnOne || view.Turns[1].Commit != turnTwo ||
		*view.HeadSHA != turnTwo || *view.BaseSHA != snapshot {
		t.Fatalf("view = %+v", view)
	}
	if view.Cumulative == nil || len(view.Cumulative.Files) != 2 || view.Cumulative.Additions != 4 || view.Cumulative.Deletions != 2 {
		t.Fatalf("cumulative = %+v", view.Cumulative)
	}
}

// TestViewIsIncompleteWithoutARecord: a turn on the chain whose record is
// missing stops the walk and says so; the tip's cumulative set still shows.
func TestViewIsIncompleteWithoutARecord(t *testing.T) {
	service, repo, rows, _ := harness(t)
	record(t, service, rows, turnOne, snapshot, 1, text(workspaceruntime.ChangeScopeTurn, "a.go", workspaceruntime.ChangeAdded))
	record(t, service, rows, turnTwo, turnOne, 2, text(workspaceruntime.ChangeScopeCumulative, "a.go", workspaceruntime.ChangeAdded))
	delete(repo.records, turnOne)
	view, err := service.View(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Complete || len(view.Turns) != 1 || view.Cumulative == nil {
		t.Fatalf("view = %+v", view)
	}
}

// TestReadsFilterWithTheCurrentPolicy: a pattern added after the record hides
// the stored file from the view and its diff from the diff read, and a policy
// that cannot be read fails closed.
func TestReadsFilterWithTheCurrentPolicy(t *testing.T) {
	service, _, rows, policy := harness(t)
	record(t, service, rows, turnOne, snapshot, 1,
		text(workspaceruntime.ChangeScopeTurn, "notes/todo.md", workspaceruntime.ChangeAdded),
		text(workspaceruntime.ChangeScopeCumulative, "notes/todo.md", workspaceruntime.ChangeAdded),
	)
	if diff, err := service.Diff(context.Background(), thread, turnOne, workspaceruntime.ChangeScopeTurn, "notes/todo.md"); err != nil || diff.Diff == "" {
		t.Fatalf("diff = %+v %v", diff, err)
	}
	policy.policy.Exclusions = append(policy.policy.Exclusions, "notes/")
	view, err := service.View(context.Background(), thread)
	if err != nil || len(view.Turns[0].Changes.Files) != 0 || view.Turns[0].Changes.Hidden != 1 || view.Cumulative.Hidden != 1 {
		t.Fatalf("view = %+v %v", view, err)
	}
	if _, err := service.Diff(context.Background(), thread, turnOne, workspaceruntime.ChangeScopeTurn, "notes/todo.md"); !errors.Is(err, workspacechanges.ErrNotFound) {
		t.Fatalf("hidden diff = %v", err)
	}
	policy.err = errors.New("settings down")
	if _, err := service.View(context.Background(), thread); !errors.Is(err, workspacechanges.ErrUnavailable) {
		t.Fatalf("no policy = %v", err)
	}
}

// TestNoHandoffReadsEmpty: a thread with no handoff (or an erased one) has no
// turns, and its diffs are not found.
func TestNoHandoffReadsEmpty(t *testing.T) {
	service, _, rows, _ := harness(t)
	rows.row = workspacehandoff.Handoff{}
	view, err := service.View(context.Background(), thread)
	if err != nil || !view.Complete || len(view.Turns) != 0 || view.HandoffID != nil {
		t.Fatalf("view = %+v %v", view, err)
	}
	if _, err := service.Diff(context.Background(), thread, turnOne, workspaceruntime.ChangeScopeTurn, "a"); !errors.Is(err, workspacechanges.ErrNotFound) {
		t.Fatalf("diff = %v", err)
	}
	if _, err := service.Diff(context.Background(), thread, turnOne, "everything", "a"); !errors.Is(err, workspacechanges.ErrInvalid) {
		t.Fatalf("bad scope = %v", err)
	}
}
