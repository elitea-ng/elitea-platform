package localturn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestCommitStoresProjectInstructions pins issue 3130 (client contract
// 1.70): whether a turn applied its folder's project instruction files is
// stored on `local_work.project_instructions`, bounded (empty files dropped,
// at most MaxReportedInstructionFiles, each cut), and a report without it
// stores none, so its digest is unchanged.
func TestCommitStoresProjectInstructions(t *testing.T) {
	store := &commitFakeStore{}
	service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := hostReportCommit()
	files := []string{"AGENTS.md", "  ", "apps/web/" + strings.Repeat("x", 500) + "/AGENTS.md"}
	for range MaxReportedInstructionFiles {
		files = append(files, "pkg/AGENTS.md")
	}
	request.Report.ProjectInstructions = &ProjectInstructionsReport{Applied: false, Files: files}
	if _, err := service.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		LocalWork struct {
			ProjectInstructions *ProjectInstructionsReport `json:"project_instructions"`
		} `json:"local_work"`
	}
	if err := json.Unmarshal(store.last.ResponseMeta, &meta); err != nil {
		t.Fatal(err)
	}
	got := meta.LocalWork.ProjectInstructions
	if got == nil {
		t.Fatal("local_work.project_instructions missing")
	}
	if got.Applied {
		t.Fatal("applied = true, want the reported false kept")
	}
	if len(got.Files) != MaxReportedInstructionFiles || got.Files[0] != "AGENTS.md" {
		t.Fatalf("files = %v, want %d with the root first and the blank one dropped", got.Files, MaxReportedInstructionFiles)
	}
	if n := len(got.Files[1]); n > MaxReportedPathBytes {
		t.Fatalf("file kept %d bytes, want at most %d", n, MaxReportedPathBytes)
	}

	encoded, err := json.Marshal(boundReport(hostReportCommit().Report))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"project_instructions"`) {
		t.Fatalf("a report without project instructions must not carry them: %s", encoded)
	}
	// A record whose files are all blank says nothing was found: dropped.
	blank := hostReportCommit().Report
	blank.ProjectInstructions = &ProjectInstructionsReport{Applied: true, Files: []string{" "}}
	if encoded, err = json.Marshal(boundReport(blank)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"project_instructions"`) {
		t.Fatalf("a record without a file must not be stored: %s", encoded)
	}
}

// TestCommitRefusesUnboundedProjectInstructions: a record with more files
// than any commit may carry is refused, as an oversized command list is.
func TestCommitRefusesUnboundedProjectInstructions(t *testing.T) {
	store := &commitFakeStore{}
	service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("b", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := hostReportCommit()
	request.Report.ProjectInstructions = &ProjectInstructionsReport{
		Applied: true, Files: make([]string, 10*MaxReportedInstructionFiles+1),
	}
	if _, err := service.Commit(context.Background(), request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
