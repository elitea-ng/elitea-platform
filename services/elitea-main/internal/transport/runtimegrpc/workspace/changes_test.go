package workspace_test

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
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
// by the Store; a stream without a header, a path that climbs out of the
// workspace or into .git, a diff over 48 KiB, a diff on a binary file and an
// unknown scope are malformed before any Store call.
func TestRecordChangesRefusesStaleAndMalformedRecords(t *testing.T) {
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
	if response := recordChanges(t, client, changesHeader(1, cloudSHA), file("a", nil)); response.GetRefusal().GetCode() !=
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH {
		t.Fatalf("a stale claim = %v", response)
	}
	if response := recordChanges(t, client, changesHeader(2, parentSHA), file("a", nil)); response.GetRefusal().GetCode() !=
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT {
		t.Fatalf("not the cloud tip = %v", response)
	}
	malformed := map[string]*runtimev1.WorkspaceChangedFileV1{
		"climbs out": file("../etc/passwd", nil),
		"into .git":  file("x/.git/config", nil),
		"absolute":   file("/etc/passwd", nil),
		"big diff": file("a", func(f *runtimev1.WorkspaceChangedFileV1) {
			f.Diff = strings.Repeat("x", 48<<10+1)
		}),
		"binary diff": file("a", func(f *runtimev1.WorkspaceChangedFileV1) { f.Binary, f.Diff = true, "+x\n" }),
		"no scope": file("a", func(f *runtimev1.WorkspaceChangedFileV1) {
			f.Scope = runtimev1.WorkspaceChangeScopeV1_WORKSPACE_CHANGE_SCOPE_V1_UNSPECIFIED
		}),
	}
	for name, bad := range malformed {
		if response := recordChanges(t, client, changesHeader(2, cloudSHA), bad); response.GetRejection().GetCode() !=
			runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
			t.Fatalf("%s = %v", name, response)
		}
	}
	if response := recordChanges(t, client, nil, file("a", nil)); response.GetRejection() == nil {
		t.Fatalf("no header = %v", response)
	}
	if len(store.changes) != 0 {
		t.Fatalf("recorded = %+v", store.changes)
	}
}
