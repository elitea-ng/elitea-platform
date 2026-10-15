package workspaceruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	// MaxPullRequestDiffBytes and MaxPullRequestDiffFiles are the GitHub
	// family's apply_git_patch bounds (decision 10): a larger diff is refused
	// before anything is written.
	MaxPullRequestDiffBytes = 1 << 20
	MaxPullRequestDiffFiles = 100
	// diffContextLines is git's default hunk context.
	diffContextLines = 3
	// binarySniffBytes is how much of a blob git reads to call it binary.
	binarySniffBytes = 8000
)

// PullRequestDiff is the unified diff from U to the cloud tip.
type PullRequestDiff struct {
	Text  []byte
	Files int
	// SHA256 is the lowercase hex digest of Text.
	SHA256 string
	Paths  []string
}

// ComputePullRequestDiff renders the unified diff (git format, the shape the
// GitHub family's patch reader takes) from upstream to cloud.
//
// It refuses before rendering anything:
//   - RefusalDiffBinary: a changed file is binary (a NUL in its first 8000
//     bytes, as git decides), a symlink or a submodule on either side;
//   - RefusalDiffTooLarge: more than 100 changed files, or a text over 1 MiB;
//   - RefusalDiffEmpty: the trees are equal.
//
// Renames are a delete plus a create; mode-only changes are not part of the
// diff (the patch path writes contents, not modes).
func ComputePullRequestDiff(ctx context.Context, upstream, cloud Tree, blobs BlobReader) (PullRequestDiff, error) {
	if blobs == nil {
		return PullRequestDiff{}, ErrInvalidRequest
	}
	paths := changedPaths(upstream, cloud)
	if len(paths) == 0 {
		return PullRequestDiff{}, Refuse(RefusalDiffEmpty)
	}
	if len(paths) > MaxPullRequestDiffFiles {
		return PullRequestDiff{}, Refuse(RefusalDiffTooLarge)
	}
	type change struct {
		path     string
		old, new []byte
		oldOK    bool
		newOK    bool
	}
	changes := make([]change, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return PullRequestDiff{}, err
		}
		before, hadBefore := upstream[path]
		after, hasAfter := cloud[path]
		for _, side := range []struct {
			file TreeFile
			ok   bool
		}{{before, hadBefore}, {after, hasAfter}} {
			if side.ok && (side.file.Mode == ModeSymlink || side.file.Mode == ModeGitlink) {
				return PullRequestDiff{}, Refuse(RefusalDiffBinary)
			}
		}
		entry := change{path: path, oldOK: hadBefore, newOK: hasAfter}
		var err error
		if hadBefore {
			if entry.old, err = readTextBlob(ctx, blobs, before.OID); err != nil {
				return PullRequestDiff{}, err
			}
		}
		if hasAfter {
			if entry.new, err = readTextBlob(ctx, blobs, after.OID); err != nil {
				return PullRequestDiff{}, err
			}
		}
		if hadBefore && hasAfter && bytes.Equal(entry.old, entry.new) {
			// Mode-only change: nothing the patch path writes.
			continue
		}
		changes = append(changes, entry)
	}
	if len(changes) == 0 {
		return PullRequestDiff{}, Refuse(RefusalDiffEmpty)
	}
	var out bytes.Buffer
	rendered := make([]string, 0, len(changes))
	for _, entry := range changes {
		writeFileDiff(&out, entry.path, entry.old, entry.new, entry.oldOK, entry.newOK)
		if out.Len() > MaxPullRequestDiffBytes {
			return PullRequestDiff{}, Refuse(RefusalDiffTooLarge)
		}
		rendered = append(rendered, entry.path)
	}
	digest := sha256.Sum256(out.Bytes())
	return PullRequestDiff{
		Text:   out.Bytes(),
		Files:  len(rendered),
		SHA256: hex.EncodeToString(digest[:]),
		Paths:  rendered,
	}, nil
}

func changedPaths(upstream, cloud Tree) []string {
	seen := make(map[string]struct{}, len(upstream)+len(cloud))
	var paths []string
	for path, before := range upstream {
		if after, ok := cloud[path]; ok && after == before {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for path := range cloud {
		if _, ok := upstream[path]; ok {
			continue
		}
		if _, ok := seen[path]; !ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func readTextBlob(ctx context.Context, blobs BlobReader, oid string) ([]byte, error) {
	if !ValidSHA1(oid) {
		return nil, ErrInvalidRequest
	}
	content, err := blobs.ReadBlob(ctx, oid, MaxPullRequestDiffBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > MaxPullRequestDiffBytes {
		return nil, Refuse(RefusalDiffTooLarge)
	}
	sniff := content
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return nil, Refuse(RefusalDiffBinary)
	}
	return content, nil
}

func writeFileDiff(out *bytes.Buffer, path string, old, new []byte, hadOld, hasNew bool) {
	fmt.Fprintf(out, "diff --git a/%s b/%s\n", path, path)
	switch {
	case !hadOld:
		out.WriteString("new file mode 100644\n")
		fmt.Fprintf(out, "--- /dev/null\n+++ b/%s\n", path)
	case !hasNew:
		out.WriteString("deleted file mode 100644\n")
		fmt.Fprintf(out, "--- a/%s\n+++ /dev/null\n", path)
	default:
		fmt.Fprintf(out, "--- a/%s\n+++ b/%s\n", path, path)
	}
	writeHunks(out, splitLines(old), splitLines(new))
}

// line is one line without its newline; last reports "no newline at end".
type line struct {
	text      string
	noNewline bool
}

func splitLines(content []byte) []line {
	if len(content) == 0 {
		return nil
	}
	text := string(content)
	trailing := strings.HasSuffix(text, "\n")
	if trailing {
		text = text[:len(text)-1]
	}
	parts := strings.Split(text, "\n")
	lines := make([]line, len(parts))
	for index, part := range parts {
		lines[index] = line{text: part}
	}
	if !trailing {
		lines[len(lines)-1].noNewline = true
	}
	return lines
}

type opKind byte

const (
	opEqual  opKind = ' '
	opDelete opKind = '-'
	opInsert opKind = '+'
)

type op struct {
	kind     opKind
	old, new int // indexes into the old and new lines (-1 when absent)
}

// diffLines is Myers' O(ND) diff; two lines are equal when their text and
// their end-of-file newline state are equal.
func diffLines(a, b []line) []op {
	n, m := len(a), len(b)
	max := n + m
	offset := max + 1
	v := make([]int, 2*max+2)
	var trace [][]int
	for d := 0; d <= max; d++ {
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, offset, d)
			}
		}
	}
	return nil
}

func backtrack(trace [][]int, a, b []line, offset, depth int) []op {
	x, y := len(a), len(b)
	var ops []op
	for d := depth; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			ops = append(ops, op{opEqual, x, y})
		}
		if x == prevX {
			y--
			ops = append(ops, op{opInsert, -1, y})
		} else {
			x--
			ops = append(ops, op{opDelete, x, -1})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		ops = append(ops, op{opEqual, x, y})
	}
	for left, right := 0, len(ops)-1; left < right; left, right = left+1, right-1 {
		ops[left], ops[right] = ops[right], ops[left]
	}
	return ops
}

func writeHunks(out *bytes.Buffer, a, b []line) {
	ops := diffLines(a, b)
	index := 0
	for index < len(ops) {
		// Find the next change.
		for index < len(ops) && ops[index].kind == opEqual {
			index++
		}
		if index >= len(ops) {
			return
		}
		start := index - diffContextLines
		if start < 0 {
			start = 0
		}
		end := index
		// Extend while changes are within 2*context of each other.
		for end < len(ops) {
			if ops[end].kind != opEqual {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == opEqual {
				run++
			}
			if run >= len(ops) || run-end > 2*diffContextLines {
				end += min(diffContextLines, run-end)
				break
			}
			end = run
		}
		writeHunk(out, ops[start:end], a, b)
		index = end
	}
}

func writeHunk(out *bytes.Buffer, ops []op, a, b []line) {
	oldStart, newStart, oldCount, newCount := -1, -1, 0, 0
	for _, entry := range ops {
		if entry.kind != opInsert {
			if oldStart < 0 {
				oldStart = entry.old
			}
			oldCount++
		}
		if entry.kind != opDelete {
			if newStart < 0 {
				newStart = entry.new
			}
			newCount++
		}
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(oldStart, oldCount, ops, true), hunkRange(newStart, newCount, ops, false))
	for _, entry := range ops {
		var current line
		switch entry.kind {
		case opInsert:
			current = b[entry.new]
		default:
			current = a[entry.old]
		}
		out.WriteByte(byte(entry.kind))
		out.WriteString(current.text)
		out.WriteByte('\n')
		if current.noNewline {
			out.WriteString("\\ No newline at end of file\n")
		}
	}
}

// hunkRange renders "start,count" with git's conventions: a zero count names
// the line before the hunk, and a count of one is written alone.
func hunkRange(start, count int, ops []op, old bool) string {
	if count == 0 {
		// The position before the insertion/deletion point.
		position := 0
		for _, entry := range ops {
			index := entry.new
			if old {
				index = entry.old
			}
			if index >= 0 {
				position = index
				break
			}
		}
		return fmt.Sprintf("%d,0", position)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}
