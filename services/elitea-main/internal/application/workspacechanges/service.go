// Package workspacechanges keeps and serves the files a cloud thread's turns
// changed: the web "Files changed" view (ADR-0034 Phase 2).
//
// The worker computes each turn's changes from its own repository right after
// it publishes the turn, and records them over the runtime plane
// (WorkspaceRecordChanges, through workspaceexecutor). Main reads no git pack:
// it bounds and stores what it is sent, and the web reads it back.
//
// A path the thread's path deny list matches, a file the send left out, and a
// path the exclusions match is never stored and never served. The one
// exception is the send's own: a file the exclusions match that the person
// explicitly included (included_excluded, when the policy allows includes)
// was uploaded, so it is shown; path_deny wins over an include. The filter
// runs on the record, with the policy of that moment, and again on every
// read, with the policy of the read, so a pattern added later hides what was
// stored before it. A rename from or to a hidden path is hidden whole.
// Hidden files are only counted.
package workspacechanges

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

var (
	// ErrNotFound: no such record or diff, or a hidden path.
	ErrNotFound = errors.New("workspace change not found")
	// ErrInvalid: a request outside the contract.
	ErrInvalid = errors.New("invalid workspace change request")
	// ErrUnavailable: a dependency did not answer; the caller may retry.
	ErrUnavailable = errors.New("workspace changes are temporarily unavailable")
)

// MaxTurns bounds the turns one read walks back from the cloud tip.
const MaxTurns = 1000

// File is one changed file as stored and served.
type File struct {
	Path      string  `json:"path"`
	FromPath  *string `json:"from_path"`
	Status    string  `json:"status"`
	Additions *int64  `json:"additions"`
	Deletions *int64  `json:"deletions"`
	Binary    bool    `json:"binary"`
	TooLarge  bool    `json:"too_large"`
	HasDiff   bool    `json:"has_diff"`
}

// Manifest is one scope of one turn, as stored.
type Manifest struct {
	Files   []File `json:"files"`
	Omitted int    `json:"omitted"`
	Hidden  int    `json:"hidden"`
}

// Diff is one file's kept diff of one scope.
type Diff struct {
	Scope string
	Path  string
	Text  string
}

// Record is one turn's changes, filtered, ready to store.
type Record struct {
	Thread            workspacehandoff.Thread
	HandoffID         string
	Commit            string
	Parent            string
	Base              string
	Seq               int64
	Depth             int64
	ExecutionID       string
	ResponseMessageID string
	Turn              Manifest
	Cumulative        Manifest
	Diffs             []Diff
}

// Stored is one recorded turn as read back.
type Stored struct {
	Commit            string
	Parent            string
	Seq               int64
	Depth             int64
	ResponseMessageID string
	CreatedAt         time.Time
	Turn              Manifest
	Cumulative        Manifest
}

// Repository stores the records (the workspace changes repository).
type Repository interface {
	// Save replaces the commit's record and its diffs, and drops the
	// cumulative diffs of every other commit of the thread.
	Save(ctx context.Context, record Record) error
	// Turns reads the handoff's records, at most limit.
	Turns(ctx context.Context, thread workspacehandoff.Thread, handoffID string, limit int) ([]Stored, error)
	// Diff reads one kept diff.
	Diff(ctx context.Context, thread workspacehandoff.Thread, commit, scope, path string) (string, bool, error)
}

// Handoffs reads a thread's handoff row.
type Handoffs interface {
	Read(ctx context.Context, thread workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error)
}

// Policies answers a project's effective policy (*workspacehandoff.Service).
type Policies interface {
	EffectivePolicy(ctx context.Context, projectID int64) (workspacehandoff.Policy, error)
}

// Service records and serves the changes.
type Service struct {
	repo     Repository
	handoffs Handoffs
	policies Policies
	logger   *slog.Logger
}

// NewService builds the service; every dependency is required.
func NewService(repo Repository, handoffs Handoffs, policies Policies, logger *slog.Logger) (*Service, error) {
	if repo == nil || handoffs == nil || policies == nil {
		return nil, errors.New("workspace changes need the repository, the handoff reader and the policy")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, handoffs: handoffs, policies: policies, logger: logger}, nil
}

// filter hides what the send did not upload: a path the deny list matches
// (always), a file the send excluded, and a path the exclusions match unless
// the send explicitly included it (workspacehandoff.checkIncludes: only when
// the policy allows includes, never a path_deny path).
type filter struct {
	deny       []string
	exclusions []string
	excluded   map[string]bool
	included   map[string]bool
}

func sentPath(name string) string { return strings.TrimPrefix(name, "./") }

func newFilter(policy workspacehandoff.Policy, handoff workspacehandoff.Handoff) filter {
	excluded := make(map[string]bool, len(handoff.Excluded))
	for _, name := range handoff.Excluded {
		excluded[strings.ToLower(sentPath(name))] = true
	}
	included := map[string]bool{}
	if policy.AllowUserInclude {
		for _, name := range handoff.IncludedExcluded {
			included[sentPath(name)] = true
		}
	}
	return filter{deny: policy.PathDeny, exclusions: policy.Exclusions, excluded: excluded, included: included}
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if workspacehandoff.MatchPathPattern(pattern, name) {
			return true
		}
	}
	return false
}

func (f filter) hidden(name string) bool {
	if name == "" {
		return false
	}
	if matchesAny(f.deny, name) {
		return true
	}
	if f.excluded[strings.ToLower(name)] && !f.included[name] {
		return true
	}
	return matchesAny(f.exclusions, name) && !f.included[name]
}

func (f filter) hiddenFile(file File) bool {
	return f.hidden(file.Path) || (file.FromPath != nil && f.hidden(*file.FromPath))
}

// apply drops the hidden files of a manifest, counting them.
func (f filter) apply(manifest Manifest) Manifest {
	out := Manifest{Files: make([]File, 0, len(manifest.Files)), Omitted: manifest.Omitted, Hidden: manifest.Hidden}
	for _, file := range manifest.Files {
		if f.hiddenFile(file) {
			out.Hidden++
			continue
		}
		out.Files = append(out.Files, file)
	}
	return out
}

func (s *Service) policyFilter(ctx context.Context, handoff workspacehandoff.Handoff) (filter, error) {
	policy, err := s.policies.EffectivePolicy(ctx, handoff.ProjectID)
	if err != nil {
		// Fail closed: without the policy nothing is known to be showable.
		return filter{}, ErrUnavailable
	}
	return newFilter(policy, handoff), nil
}

// Record keeps the running turn's changes. The record was checked once, at
// the transport boundary (workspaceruntime.SanitizeTurnChanges); the caller
// (the executor adapter) has resolved the claim to handoff and checked that
// changes.Commit is its cloud tip and changes.Base its snapshot.
func (s *Service) Record(ctx context.Context, handoff workspacehandoff.Handoff, executionID string, seq int64,
	changes workspaceruntime.TurnChanges) error {
	f, err := s.policyFilter(ctx, handoff)
	if err != nil {
		return err
	}
	record := Record{
		Thread:            workspacehandoff.Thread{ProjectID: handoff.ProjectID, ConversationUUID: handoff.ConversationUUID},
		HandoffID:         handoff.HandoffID,
		Commit:            changes.Commit,
		Parent:            changes.Parent,
		Base:              changes.Base,
		Seq:               seq,
		Depth:             int64(changes.Depth),
		ExecutionID:       executionID,
		ResponseMessageID: handoff.RunningResponseID,
		Turn:              Manifest{Files: []File{}, Omitted: int(changes.TurnOmitted)},
		Cumulative:        Manifest{Files: []File{}, Omitted: int(changes.CumulativeOmitted)},
	}
	for _, change := range changes.Files {
		file := fileOf(change)
		target := &record.Turn
		if change.Scope == workspaceruntime.ChangeScopeCumulative {
			target = &record.Cumulative
		}
		if f.hiddenFile(file) {
			// Neither the path nor its diff is kept.
			target.Hidden++
			continue
		}
		target.Files = append(target.Files, file)
		if file.HasDiff {
			record.Diffs = append(record.Diffs, Diff{Scope: change.Scope, Path: change.Path, Text: change.Diff})
		}
	}
	if err := s.repo.Save(ctx, record); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The handoff is no longer the thread's (erased, sent again).
			return ErrNotFound
		}
		s.logger.ErrorContext(ctx, "workspace changes: save", "conversation_uuid", handoff.ConversationUUID, "err", err)
		return ErrUnavailable
	}
	return nil
}

func fileOf(change workspaceruntime.ChangedFile) File {
	file := File{Path: change.Path, Status: change.Status, Binary: change.Binary, TooLarge: change.TooLarge, HasDiff: change.Diff != ""}
	if change.Status == workspaceruntime.ChangeRenamed {
		from := change.FromPath
		file.FromPath = &from
	}
	if !change.Binary && !change.TooLarge && change.Additions != nil && change.Deletions != nil {
		added, removed := int64(*change.Additions), int64(*change.Deletions)
		file.Additions, file.Deletions = &added, &removed
	}
	return file
}
