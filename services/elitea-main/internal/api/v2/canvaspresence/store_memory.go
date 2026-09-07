package canvaspresence

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is the single-replica Store. It is the DEFAULT so that a
// deployment with no Redis still serves the route correctly for one replica,
// rather than answering 500 — the same direction internal/api/v2/conversations
// takes when its object store is absent.
//
// Its limit is the one NewHandler states: two replicas hold two rosters.
type MemoryStore struct {
	mu      sync.Mutex
	rosters map[string]map[string]entry
	now     func() time.Time
}

// NewMemoryStore returns an empty in-process store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rosters: map[string]map[string]entry{}, now: time.Now}
}

// SetClock replaces time.Now for tests that walk a roster past its TTL without
// sleeping.
func (s *MemoryStore) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *MemoryStore) Touch(_ context.Context, key string, editor Editor, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	roster, ok := s.rosters[key]
	if !ok {
		roster = map[string]entry{}
		s.rosters[key] = roster
	}
	roster[editor.UserID] = entry{editor: editor, deadline: s.now().Add(ttl)}
	return nil
}

func (s *MemoryStore) Remove(_ context.Context, key string, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	roster, ok := s.rosters[key]
	if !ok {
		return nil
	}
	delete(roster, userID)
	if len(roster) == 0 {
		delete(s.rosters, key)
	}
	return nil
}

// List drops every entry whose deadline has passed BEFORE returning, so an
// expired editor is gone from the answer and from the store in one step. This
// is what removes the reference's need for a `* * * * *` sweeper.
func (s *MemoryStore) List(_ context.Context, key string) ([]Editor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	roster, ok := s.rosters[key]
	if !ok {
		return []Editor{}, nil
	}
	now := s.now()
	live := make([]Editor, 0, len(roster))
	for userID, held := range roster {
		if !held.deadline.After(now) {
			delete(roster, userID)
			continue
		}
		live = append(live, held.editor)
	}
	if len(roster) == 0 {
		delete(s.rosters, key)
	}
	return sortRoster(live), nil
}
