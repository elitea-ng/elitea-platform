package workspacechanges

import (
	"context"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

// Set is one scope as served: the shown files and their counted lines.
type Set struct {
	Files     []File `json:"files"`
	Omitted   int    `json:"omitted_files"`
	Hidden    int    `json:"hidden_files"`
	Additions int64  `json:"additions"`
	Deletions int64  `json:"deletions"`
}

// Turn is one cloud turn of the live chain.
type Turn struct {
	Commit            string    `json:"commit"`
	Parent            string    `json:"parent_sha"`
	Seq               int64     `json:"seq"`
	Number            *int64    `json:"number"`
	ResponseMessageID *string   `json:"response_message_id"`
	CreatedAt         time.Time `json:"created_at"`
	Changes           Set       `json:"changes"`
}

// View is getWorkspaceHandoffChanges' answer.
type View struct {
	HandoffID  *string `json:"handoff_id"`
	BaseSHA    *string `json:"base_sha"`
	HeadSHA    *string `json:"head_sha"`
	Complete   bool    `json:"complete"`
	Cumulative *Set    `json:"cumulative"`
	// CumulativeCommit is the commit the cumulative set (and its diffs)
	// belongs to: head_sha, or the newest recorded turn when the tip has no
	// record.
	CumulativeCommit *string `json:"cumulative_commit"`
	Turns            []Turn  `json:"turns"`
}

func setOf(manifest Manifest) Set {
	out := Set{Files: manifest.Files, Omitted: manifest.Omitted, Hidden: manifest.Hidden}
	if out.Files == nil {
		out.Files = []File{}
	}
	for _, file := range out.Files {
		if file.Additions != nil {
			out.Additions += *file.Additions
		}
		if file.Deletions != nil {
			out.Deletions += *file.Deletions
		}
	}
	return out
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// readable answers the thread's handoff when it has a cloud copy to read.
func (s *Service) readable(ctx context.Context, thread workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error) {
	handoff, ok, err := s.handoffs.Read(ctx, thread)
	if err != nil {
		s.logger.ErrorContext(ctx, "workspace changes: read handoff", "conversation_uuid", thread.ConversationUUID, "err", err)
		return workspacehandoff.Handoff{}, false, ErrUnavailable
	}
	if !ok || handoff.DeletedAt != nil || handoff.HandoffSHA == nil || handoff.HandoffID == "" {
		return workspacehandoff.Handoff{}, false, nil
	}
	return handoff, true, nil
}

// View answers the live chain's turns, oldest first, and the cumulative set
// of its tip, filtered with the policy of now.
func (s *Service) View(ctx context.Context, thread workspacehandoff.Thread) (View, error) {
	view := View{Complete: true, Turns: []Turn{}}
	handoff, ok, err := s.readable(ctx, thread)
	if err != nil || !ok {
		return view, err
	}
	view.HandoffID, view.BaseSHA = optional(handoff.HandoffID), optional(*handoff.HandoffSHA)
	if handoff.CloudSHA == nil || *handoff.CloudSHA == *handoff.HandoffSHA {
		return view, nil
	}
	view.HeadSHA = optional(*handoff.CloudSHA)
	f, err := s.policyFilter(ctx, handoff)
	if err != nil {
		return View{}, err
	}
	stored, err := s.repo.Turns(ctx, thread, handoff.HandoffID, MaxTurns)
	if err != nil {
		s.logger.ErrorContext(ctx, "workspace changes: read turns", "conversation_uuid", thread.ConversationUUID, "err", err)
		return View{}, ErrUnavailable
	}
	view.Turns = liveChain(stored, *handoff.CloudSHA, *handoff.HandoffSHA, f, &view)
	return view, nil
}

// liveChain walks the parents from the cloud tip to the snapshot: only the
// live chain, so a regenerated answer's earlier attempt is not listed. It
// answers the turns oldest first and fills view's cumulative set and
// completeness.
//
// A commit with no record (its record failed) does not stop the walk. Its
// parent is unknown to Main (it reads no git pack), so the walk resumes at
// the record of the live turn below it: the one with the highest turn
// number under the missing commit's, and, of the attempts at that number,
// the newest recorded before the turn above (a regenerate replaces only the
// newest turn, so the newest attempt below a live turn is the live one).
// When the tip itself has no record, the walk starts at the newest record.
// Either way the view is not complete.
func liveChain(stored []Stored, tip, base string, f filter, view *View) []Turn {
	if len(stored) == 0 {
		view.Complete = false
		return []Turn{}
	}
	byCommit := make(map[string]Stored, len(stored))
	for _, turn := range stored {
		byCommit[turn.Commit] = turn
	}
	current, found := byCommit[tip]
	if !found {
		view.Complete = false
		current = newest(stored, func(Stored) bool { return true })
	}
	cumulative := setOf(f.apply(current.Cumulative))
	view.Cumulative, view.CumulativeCommit = &cumulative, optional(current.Commit)
	walked := map[string]bool{}
	chain := []Turn{}
	for len(chain) < len(stored) {
		walked[current.Commit] = true
		chain = append(chain, Turn{
			Commit: current.Commit, Parent: current.Parent, Seq: current.Seq, Number: number(current.Depth),
			ResponseMessageID: optional(current.ResponseMessageID), CreatedAt: current.CreatedAt, Changes: setOf(f.apply(current.Turn)),
		})
		if current.Parent == base {
			break
		}
		if parent, ok := byCommit[current.Parent]; ok && !walked[parent.Commit] {
			current = parent
			continue
		}
		view.Complete = false
		above := current
		next := newest(stored, func(turn Stored) bool {
			if walked[turn.Commit] || turn.Seq >= above.Seq {
				return false
			}
			// The missing parent's number is above.Depth-1; the next record
			// is under it.
			return above.Depth <= 0 || (turn.Depth > 0 && turn.Depth < above.Depth-1)
		})
		if next.Commit == "" {
			break
		}
		current = next
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return chain
}

// newest answers the record that keep accepts with the highest turn number,
// then the highest seq; the zero Stored when none.
func newest(stored []Stored, keep func(Stored) bool) Stored {
	var best Stored
	for _, turn := range stored {
		if !keep(turn) {
			continue
		}
		if best.Commit == "" || turn.Depth > best.Depth || (turn.Depth == best.Depth && turn.Seq > best.Seq) {
			best = turn
		}
	}
	return best
}

func number(depth int64) *int64 {
	if depth <= 0 {
		return nil
	}
	return &depth
}

// DiffView is getWorkspaceHandoffDiff's answer.
type DiffView struct {
	Commit string `json:"commit"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Diff   string `json:"diff"`
}

// Diff answers one kept diff; ErrNotFound for a hidden path too.
func (s *Service) Diff(ctx context.Context, thread workspacehandoff.Thread, commit, scope, path string) (DiffView, error) {
	if !workspaceruntime.ValidSHA1(commit) || !workspaceruntime.ValidChangedPath(path) ||
		(scope != workspaceruntime.ChangeScopeTurn && scope != workspaceruntime.ChangeScopeCumulative) {
		return DiffView{}, ErrInvalid
	}
	handoff, ok, err := s.readable(ctx, thread)
	if err != nil {
		return DiffView{}, err
	}
	if !ok {
		return DiffView{}, ErrNotFound
	}
	f, err := s.policyFilter(ctx, handoff)
	if err != nil {
		return DiffView{}, err
	}
	if f.hidden(path) {
		return DiffView{}, ErrNotFound
	}
	text, found, err := s.repo.Diff(ctx, thread, commit, scope, path)
	if err != nil {
		s.logger.ErrorContext(ctx, "workspace changes: read diff", "conversation_uuid", thread.ConversationUUID, "err", err)
		return DiffView{}, ErrUnavailable
	}
	if !found {
		return DiffView{}, ErrNotFound
	}
	return DiffView{Commit: commit, Scope: scope, Path: path, Diff: text}, nil
}
