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
	Turns      []Turn  `json:"turns"`
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
	byCommit := make(map[string]Stored, len(stored))
	for _, turn := range stored {
		byCommit[turn.Commit] = turn
	}
	// Walk the parents from the tip to the snapshot: only the live chain, so a
	// regenerated answer's earlier attempt is not listed.
	chain := []Turn{}
	commit := *handoff.CloudSHA
	for steps := 0; commit != *handoff.HandoffSHA; steps++ {
		turn, found := byCommit[commit]
		if !found || steps >= len(stored) {
			view.Complete = false
			break
		}
		if len(chain) == 0 {
			cumulative := setOf(f.apply(turn.Cumulative))
			view.Cumulative = &cumulative
		}
		chain = append(chain, Turn{
			Commit: turn.Commit, Parent: turn.Parent, Seq: turn.Seq, ResponseMessageID: optional(turn.ResponseMessageID),
			CreatedAt: turn.CreatedAt, Changes: setOf(f.apply(turn.Turn)),
		})
		commit = turn.Parent
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	view.Turns = chain
	return view, nil
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
