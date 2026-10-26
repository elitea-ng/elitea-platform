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
	Files             []ChangedFile
}

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

// ValidateTurnChanges checks a record against the contract's shape and
// bounds; ErrInvalidRequest otherwise.
func ValidateTurnChanges(changes TurnChanges) error {
	if !ValidSHA1(changes.Commit) || !ValidSHA1(changes.Parent) || !ValidSHA1(changes.Base) ||
		changes.TurnOmitted > 1<<20 || changes.CumulativeOmitted > 1<<20 {
		return ErrInvalidRequest
	}
	type key struct{ scope, path string }
	seen := make(map[key]bool, len(changes.Files))
	count := map[string]int{}
	diffBytes := map[string]int{}
	for _, file := range changes.Files {
		if file.Scope != ChangeScopeTurn && file.Scope != ChangeScopeCumulative {
			return ErrInvalidRequest
		}
		if !ValidChangedPath(file.Path) || seen[key{file.Scope, file.Path}] {
			return ErrInvalidRequest
		}
		seen[key{file.Scope, file.Path}] = true
		switch file.Status {
		case ChangeRenamed:
			if !ValidChangedPath(file.FromPath) || file.FromPath == file.Path {
				return ErrInvalidRequest
			}
		case ChangeAdded, ChangeModified, ChangeDeleted:
			if file.FromPath != "" {
				return ErrInvalidRequest
			}
		default:
			return ErrInvalidRequest
		}
		if len(file.Diff) > MaxFileDiffBytes || !utf8.ValidString(file.Diff) || strings.ContainsRune(file.Diff, 0) {
			return ErrInvalidRequest
		}
		if file.Diff != "" && (file.Binary || file.TooLarge) {
			return ErrInvalidRequest
		}
		count[file.Scope]++
		diffBytes[file.Scope] += len(file.Diff)
		if count[file.Scope] > MaxChangedFiles || diffBytes[file.Scope] > MaxScopeDiffBytes {
			return ErrInvalidRequest
		}
	}
	return nil
}
