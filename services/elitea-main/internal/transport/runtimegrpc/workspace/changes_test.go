package workspace_test

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	parentSHA = "2222222222222222222222222222222222222222"
	baseSHA   = "3333333333333333333333333333333333333333"
)

func recordChanges(t *testing.T, client runtimev1.WorkspaceStoreServiceClient, header *runtimev1.WorkspaceChangesHeaderV1,
	files ...*runtimev1.WorkspaceChangedFileV1) *runtimev1.WorkspaceRecordChangesResponseV1 {
	t.Helper()
	stream, err := client.WorkspaceRecordChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if header != nil {
		if err := stream.Send(&runtimev1.WorkspaceChangesFrameV1{Frame: &runtimev1.WorkspaceChangesFrameV1_Header{Header: header}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		if err := stream.Send(&runtimev1.WorkspaceChangesFrameV1{Frame: &runtimev1.WorkspaceChangesFrameV1_File{File: file}}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func changesHeader(epoch uint64, commit string) *runtimev1.WorkspaceChangesHeaderV1 {
	return &runtimev1.WorkspaceChangesHeaderV1{Claim: claim(epoch), Commit: commit, Parent: parentSHA, Base: baseSHA, TurnOmitted: 2}
}

func counted(value uint32) *uint32 { return &value }

// TestRecordChangesKeepsTheTurnsFilesAtTheFrameBound: the largest file frame
// the contract allows (two 4 KiB paths, a 48 KiB diff) passes the control
// listener's 64 KiB request bound, and the record reaches the Store whole.
func TestRecordChangesKeepsTheTurnsFilesAtTheFrameBound(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	longPath := strings.Repeat("a/", 2047) + "b"
	response := recordChanges(t, client, changesHeader(2, cloudSHA),
		&runtimev1.WorkspaceChangedFileV1{
			Scope: runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_TURN, Path: "src/a.rs",
			Status: runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_MODIFIED, Additions: counted(1), Deletions: counted(0),
			Diff: strings.Repeat("+x\n", 48<<10/3),
		},
		&runtimev1.WorkspaceChangedFileV1{
			Scope: runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_CUMULATIVE, Path: longPath, FromPath: longPath + "c",
			Status: runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_RENAMED,
		},
		&runtimev1.WorkspaceChangedFileV1{
			Scope: runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_CUMULATIVE, Path: "logo.png", Binary: true,
			Status: runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_ADDED,
		},
	)
	if !response.GetRecorded() {
		t.Fatalf("record = %v", response)
	}
	if len(store.changes) != 1 || len(store.changes[0].Files) != 3 || store.changes[0].TurnOmitted != 2 ||
		store.changes[0].Files[1].FromPath != longPath+"c" || *store.changes[0].Files[0].Additions != 1 ||
		store.changes[0].Files[2].Additions != nil {
		t.Fatalf("recorded = %+v", store.changes)
	}
}

// TestRecordChangesRefusesStaleAndMalformedRecords: a stale claim is refused
// by the Store, a header with a bad commit or a stream without a header is
// malformed, and a claim the fence rejects is answered on the header alone,
// before a file frame is judged.
func TestRecordChangesRefusesStaleAndMalformedRecords(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	good := &runtimev1.WorkspaceChangedFileV1{
		Scope: runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_TURN, Path: "a",
		Status: runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_ADDED,
	}
	if response := recordChanges(t, client, changesHeader(1, cloudSHA), good); response.GetRefusal().GetCode() !=
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH {
		t.Fatalf("a stale claim = %v", response)
	}
	if response := recordChanges(t, client, changesHeader(2, parentSHA), good); response.GetRefusal().GetCode() !=
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT {
		t.Fatalf("not the cloud tip = %v", response)
	}
	if response := recordChanges(t, client, changesHeader(2, "not-a-commit"), good); response.GetRejection().GetCode() !=
		runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
		t.Fatalf("bad commit = %v", response)
	}
	if response := recordChanges(t, client, nil, good); response.GetRejection() == nil {
		t.Fatalf("no header = %v", response)
	}
	fenced := dial(t, store, verifier{err: runtimedomain.ErrStaleFence})
	header := &runtimev1.WorkspaceChangesFrameV1{Frame: &runtimev1.WorkspaceChangesFrameV1_Header{Header: changesHeader(2, cloudSHA)}}
	stream, err := fenced.WorkspaceRecordChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(header); err != nil {
		t.Fatal(err)
	}
	// The answer comes without the client closing its side: the server stops
	// at the header.
	var response runtimev1.WorkspaceRecordChangesResponseV1
	if err := stream.RecvMsg(&response); err != nil || response.GetRejection() == nil ||
		response.GetRejection().GetCode() == runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
		t.Fatalf("fenced = %v %v", &response, err)
	}
	if len(store.changes) != 0 {
		t.Fatalf("recorded = %+v", store.changes)
	}
}

// TestRecordChangesDropsABadFileAlone: a path that climbs out of the
// workspace or into .git, an unknown scope or status, and a repeated path
// are dropped and counted as omitted in their scope; a diff Main cannot keep
// (over 48 KiB, on a binary file) is dropped and the file kept. The rest of
// the record is kept.
func TestRecordChangesDropsABadFileAlone(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	file := func(path string, mutate func(*runtimev1.WorkspaceChangedFileV1)) *runtimev1.WorkspaceChangedFileV1 {
		out := &runtimev1.WorkspaceChangedFileV1{
			Scope: runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_TURN, Path: path,
			Status: runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_ADDED,
		}
		if mutate != nil {
			mutate(out)
		}
		return out
	}
	response := recordChanges(t, client, changesHeader(2, cloudSHA),
		file("src/ok.rs", func(f *runtimev1.WorkspaceChangedFileV1) { f.Diff = "+x\n" }),
		file("../etc/passwd", nil),
		file("x/.GIT/config", nil),
		file("/etc/passwd", nil),
		file("src/ok.rs", nil),
		file("b", func(f *runtimev1.WorkspaceChangedFileV1) {
			f.Status = runtimev1.WorkspaceChangeStatusV1_WORKSPACE_CHANGE_STATUS_V1_UNSPECIFIED
		}),
		file("c", func(f *runtimev1.WorkspaceChangedFileV1) {
			f.Scope = runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_UNSPECIFIED
		}),
		file("big", func(f *runtimev1.WorkspaceChangedFileV1) { f.Diff = strings.Repeat("x", 48<<10+1) }),
		file("logo.png", func(f *runtimev1.WorkspaceChangedFileV1) { f.Binary, f.Diff = true, "+x\n" }),
	)
	if !response.GetRecorded() || len(store.changes) != 1 {
		t.Fatalf("record = %v %+v", response, store.changes)
	}
	got := store.changes[0]
	// changesHeader counts 2 omitted; five entries are dropped in the turn scope.
	if got.TurnOmitted != 7 || len(got.Files) != 3 || got.Files[0].Path != "src/ok.rs" || got.Files[0].Diff != "+x\n" ||
		got.Files[1].Path != "big" || got.Files[1].Diff != "" || got.Files[2].Path != "logo.png" || got.Files[2].Diff != "" {
		t.Fatalf("recorded = %+v", got)
	}
}
