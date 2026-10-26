package workspaceruntime

import (
	"path"
	"strings"
	"unicode/utf8"
)

// The files a cloud turn changed (ADR-0034 Phase 2, the web Files changed
// view), as the worker records them with WorkspaceRecordChanges after its
// publish. The worker computes them from its own repository; Main reads no git
// pack and only bounds, filters and stores what it is sent.

// Change scopes.
const (
	// ChangeScopeTurn: the turn's parent tree to its commit's tree.
	ChangeScopeTurn = "turn"
	// ChangeScopeCumulative: the handoff snapshot's tree to the commit's tree.
	ChangeScopeCumulative = "cumulative"
)

// Change statuses.
const (
	ChangeAdded    = "added"
	ChangeModified = "modified"
	ChangeDeleted  = "deleted"
	// ChangeRenamed is an exact rename: the same blob left FromPath.
	ChangeRenamed = "renamed"
)

// The bounds of one turn's record (the worker holds the same ones).
const (
	// MaxChangedFiles bounds the files listed per scope; the rest are counted.
	MaxChangedFiles = 1000
	// MaxChangedPathBytes bounds a path and a rename's from-path.
	MaxChangedPathBytes = 4096
	// MaxFileDiffBytes bounds one file's kept diff (one frame stays under the
	// control listener's 64 KiB request bound).
	MaxFileDiffBytes = 48 << 10
	// MaxScopeDiffBytes bounds the diffs kept for one scope of one turn.
	MaxScopeDiffBytes = 256 << 10
)

// ChangedFile is one changed file of one scope. Additions and Deletions are
// nil when the worker did not count them (binary, too large).
type ChangedFile struct {
	Scope     string
	Path      string
	FromPath  string
	Status    string
	Additions *uint32
	Deletions *uint32
	Binary    bool
	TooLarge  bool
	Diff      string
}

// TurnChanges is one turn's record: the published commit, its parent, the
// handoff snapshot, and the files of both scopes.
type TurnChanges struct {
	Commit            string
	Parent            string
	Base              string
	TurnOmitted       uint32
	CumulativeOmitted uint32
	// Depth is the commit's turn number from the handoff snapshot (1: the
	// first cloud turn); 0 when the worker could not tell.
	Depth uint32
	Files []ChangedFile
}

// MaxChangeDepth bounds a recorded turn number.
const MaxChangeDepth = 1 << 20

// ValidChangedPath reports a workspace-relative, slash-separated path with no
// empty, `.` or `..` segment, no NUL and no `.git` component: what a git tree
// can hold.
func ValidChangedPath(name string) bool {
	if name == "" || len(name) > MaxChangedPathBytes || !utf8.ValidString(name) ||
		strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "." || segment == ".." || strings.EqualFold(segment, ".git") {
			return false
		}
	}
	return true
}

// ValidateTurnChanges checks a record's header (the three commits, the
// counts); ErrInvalidRequest otherwise. The files are not checked here:
// SanitizeTurnChanges drops a bad one alone.
func ValidateTurnChanges(changes TurnChanges) error {
	if !ValidSHA1(changes.Commit) || !ValidSHA1(changes.Parent) || !ValidSHA1(changes.Base) ||
		changes.TurnOmitted > 1<<20 || changes.CumulativeOmitted > 1<<20 || changes.Depth > MaxChangeDepth {
		return ErrInvalidRequest
	}
	return nil
}

// validChangedFile reports a file entry Main can keep: a known scope and
// status, valid paths, and a rename's from-path only on a rename.
func validChangedFile(file ChangedFile) bool {
	if file.Scope != ChangeScopeTurn && file.Scope != ChangeScopeCumulative {
		return false
	}
	if !ValidChangedPath(file.Path) {
		return false
	}
	switch file.Status {
	case ChangeRenamed:
		return ValidChangedPath(file.FromPath) && file.FromPath != file.Path
	case ChangeAdded, ChangeModified, ChangeDeleted:
		return file.FromPath == ""
	default:
		return false
	}
}

// SanitizeTurnChanges is the record's one check, at the transport boundary
// (the gRPC handler): the header must be valid (ErrInvalidRequest), and each
// file is judged alone, so one bad entry never loses the whole record. An
// entry Main cannot keep, a repeated path, or a file past the scope's file
// bound is dropped and counted as omitted in its scope; a diff Main cannot
// keep (too large, past the scope's diff budget, not text, or on a binary or
// too large file) is dropped and the file kept without one.
func SanitizeTurnChanges(changes TurnChanges) (TurnChanges, error) {
	if err := ValidateTurnChanges(changes); err != nil {
		return TurnChanges{}, err
	}
	out := changes
	out.Files = make([]ChangedFile, 0, len(changes.Files))
	type key struct{ scope, path string }
	seen := make(map[key]bool, len(changes.Files))
	count := map[string]int{}
	diffBytes := map[string]int{}
	omit := func(scope string) {
		switch scope {
		case ChangeScopeTurn:
			out.TurnOmitted++
		case ChangeScopeCumulative:
			out.CumulativeOmitted++
		}
	}
	for _, file := range changes.Files {
		if !validChangedFile(file) || seen[key{file.Scope, file.Path}] || count[file.Scope] >= MaxChangedFiles {
			omit(file.Scope)
			continue
		}
		seen[key{file.Scope, file.Path}] = true
		count[file.Scope]++
		if file.Diff != "" && (file.Binary || file.TooLarge || len(file.Diff) > MaxFileDiffBytes ||
			!utf8.ValidString(file.Diff) || strings.ContainsRune(file.Diff, 0) ||
			diffBytes[file.Scope]+len(file.Diff) > MaxScopeDiffBytes) {
			file.Diff = ""
		}
		diffBytes[file.Scope] += len(file.Diff)
		out.Files = append(out.Files, file)
	}
	return out, nil
}
