package workspaceruntime

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type memoryBlobs map[string][]byte

func (m memoryBlobs) ReadBlob(_ context.Context, oid string, limit int64) ([]byte, error) {
	content, ok := m[oid]
	if !ok {
		return nil, errors.New("missing blob")
	}
	if int64(len(content)) > limit+1 {
		return content[:limit+1], nil
	}
	return content, nil
}

func (m memoryBlobs) add(content string) string {
	digest := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
	oid := hex.EncodeToString(digest[:])
	m[oid] = []byte(content)
	return oid
}

func file(blobs memoryBlobs, content string) TreeFile {
	return TreeFile{Mode: ModeFile, OID: blobs.add(content)}
}

func numbered(from, to int) string {
	var out strings.Builder
	for index := from; index <= to; index++ {
		fmt.Fprintf(&out, "line %d\n", index)
	}
	return out.String()
}

func TestPullRequestDiffRendersCreateModifyDelete(t *testing.T) {
	blobs := memoryBlobs{}
	upstream := Tree{
		"README.md":    file(blobs, "hello\nworld\n"),
		"gone.txt":     file(blobs, "bye\n"),
		"same.txt":     file(blobs, "unchanged\n"),
		"src/main.go":  file(blobs, numbered(1, 30)),
		"mode-only.sh": file(blobs, "echo\n"),
	}
	modeOnly := upstream["mode-only.sh"]
	modeOnly.Mode = ModeExecutable
	changed := strings.Replace(numbered(1, 30), "line 5\n", "line five\n", 1)
	changed = strings.Replace(changed, "line 25\n", "line twenty-five\nline extra\n", 1)
	cloud := Tree{
		"README.md":    file(blobs, "hello\nthere\nworld"),
		"new/file.txt": file(blobs, "a\nb\n"),
		"same.txt":     upstream["same.txt"],
		"src/main.go":  file(blobs, changed),
		"mode-only.sh": modeOnly,
	}
	diff, err := ComputePullRequestDiff(context.Background(), upstream, cloud, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Files != 4 || strings.Join(diff.Paths, ",") != "README.md,gone.txt,new/file.txt,src/main.go" {
		t.Fatalf("files %d paths %v", diff.Files, diff.Paths)
	}
	text := string(diff.Text)
	for _, want := range []string{
		"diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
		"diff --git a/new/file.txt b/new/file.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new/file.txt\n@@ -0,0 +1,2 @@\n+a\n+b\n",
		"+there\n",
		"\\ No newline at end of file\n",
		"-line 5\n+line five\n",
		"+line extra\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff lacks %q:\n%s", want, text)
		}
	}
	// Two separate hunks for main.go (the changes are 20 lines apart).
	section := text[strings.Index(text, "diff --git a/src/main.go"):]
	if strings.Count(section, "@@ -") != 2 {
		t.Fatalf("want two hunks:\n%s", section)
	}
	if len(diff.SHA256) != 64 {
		t.Fatal("digest missing")
	}
	assertGitApplies(t, upstream, cloud, blobs, diff.Text)
}

func TestPullRequestDiffRefusesBinaryBeforeRendering(t *testing.T) {
	blobs := memoryBlobs{}
	upstream := Tree{"a.txt": file(blobs, "a\n")}
	cloud := Tree{"a.txt": file(blobs, "b\n"), "logo.png": file(blobs, "\x89PNG\x00\x01")}
	_, err := ComputePullRequestDiff(context.Background(), upstream, cloud, blobs)
	if refusal, ok := AsRefusal(err); !ok || refusal.Code != RefusalDiffBinary {
		t.Fatalf("want binary refusal, got %v", err)
	}
	link := Tree{"a.txt": upstream["a.txt"], "link": {Mode: ModeSymlink, OID: blobs.add("a.txt")}}
	if refusal, ok := AsRefusal(func() error {
		_, err := ComputePullRequestDiff(context.Background(), upstream, link, blobs)
		return err
	}()); !ok || refusal.Code != RefusalDiffBinary {
		t.Fatal("a symlink is not a text change")
	}
}

func TestPullRequestDiffRefusesTooManyFilesAndTooManyBytes(t *testing.T) {
	blobs := memoryBlobs{}
	cloud := Tree{}
	for index := 0; index <= MaxPullRequestDiffFiles; index++ {
		cloud[fmt.Sprintf("f%03d.txt", index)] = file(blobs, "x\n")
	}
	_, err := ComputePullRequestDiff(context.Background(), Tree{}, cloud, blobs)
	if refusal, ok := AsRefusal(err); !ok || refusal.Code != RefusalDiffTooLarge {
		t.Fatalf("want too large (files), got %v", err)
	}
	big := strings.Repeat("0123456789abcdef\n", (MaxPullRequestDiffBytes/17)+10)
	_, err = ComputePullRequestDiff(context.Background(), Tree{}, Tree{"big.txt": file(blobs, big)}, blobs)
	if refusal, ok := AsRefusal(err); !ok || refusal.Code != RefusalDiffTooLarge {
		t.Fatalf("want too large (bytes), got %v", err)
	}
}

func TestPullRequestDiffRefusesEqualTrees(t *testing.T) {
	blobs := memoryBlobs{}
	tree := Tree{"a.txt": file(blobs, "a\n")}
	_, err := ComputePullRequestDiff(context.Background(), tree, tree, blobs)
	if refusal, ok := AsRefusal(err); !ok || refusal.Code != RefusalDiffEmpty {
		t.Fatalf("want empty, got %v", err)
	}
}

func TestDiffLinesRoundTripsManyEdits(t *testing.T) {
	blobs := memoryBlobs{}
	old := numbered(1, 200)
	updated := old
	for _, index := range []int{1, 2, 50, 51, 52, 120, 199, 200} {
		updated = strings.Replace(updated, fmt.Sprintf("line %d\n", index), fmt.Sprintf("edited %d\nadded %d\n", index, index), 1)
	}
	updated = strings.Replace(updated, "line 80\nline 81\nline 82\n", "", 1)
	upstream := Tree{"f.txt": file(blobs, old)}
	cloud := Tree{"f.txt": file(blobs, updated)}
	diff, err := ComputePullRequestDiff(context.Background(), upstream, cloud, blobs)
	if err != nil {
		t.Fatal(err)
	}
	assertGitApplies(t, upstream, cloud, blobs, diff.Text)
}

// assertGitApplies applies the diff with real git (when installed) to the
// upstream tree and compares the result with the cloud tree.
func assertGitApplies(t *testing.T, upstream, cloud Tree, blobs memoryBlobs, patch []byte) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Log("git is not installed; the round trip through git apply is not checked")
		return
	}
	dir := t.TempDir()
	for path, entry := range upstream {
		target := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, blobs[entry.OID], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	patchFile := filepath.Join(t.TempDir(), "change.patch")
	if err := os.WriteFile(patchFile, patch, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(gitPath, "apply", "--unsafe-paths", "--directory=", patchFile)
	command.Dir = dir
	command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + dir}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s\n%s", err, output, patch)
	}
	for path, entry := range cloud {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(blobs[entry.OID]) {
			t.Fatalf("%s differs after apply:\n%q\n%q", path, got, blobs[entry.OID])
		}
	}
	for path := range upstream {
		if _, ok := cloud[path]; !ok {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err == nil {
				t.Fatalf("%s should be deleted", path)
			}
		}
	}
}
