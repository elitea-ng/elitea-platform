package egressaudit

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is a Store that keeps the rows in one process. It is for tests:
// two Mergers over ONE MemoryStore behave as two Main replicas over one
// database, and the contract tests run the same cases over it and over the
// Postgres store. A deployment composes the Postgres store.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[rowKey]*memoryRow
}

type rowKey struct{ execution, replica string }

type memoryRow struct {
	Row
	audited bool
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: map[rowKey]*memoryRow{}}
}

// Upsert implements Store.
func (s *MemoryStore) Upsert(_ context.Context, report Report, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	final := report.Final || report.ReplicaShutdown
	report.Final, report.ReplicaShutdown = final, false
	key := rowKey{report.ExecutionID, report.ReplicaID}
	old, ok := s.rows[key]
	switch {
	case !ok || old.audited:
		s.rows[key] = &memoryRow{Row: Row{Report: report, FirstReport: now, UpdatedAt: now}}
	case old.Final && !final:
		// A late non-final snapshot does not undo the replica's last report.
	default:
		old.Row = Row{Report: report, FirstReport: old.FirstReport, UpdatedAt: now}
	}
	return nil
}

// ClaimDue implements Store.
func (s *MemoryStore) ClaimDue(_ context.Context, now time.Time, settle, maxHold time.Duration) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type turn struct {
		rows     []*memoryRow
		allFinal bool
		last     time.Time
		expires  time.Time
	}
	turns := map[string]*turn{}
	for _, row := range s.rows {
		if row.audited {
			continue
		}
		t, ok := turns[row.ExecutionID]
		if !ok {
			t = &turn{allFinal: true}
			turns[row.ExecutionID] = t
		}
		t.rows = append(t.rows, row)
		t.allFinal = t.allFinal && row.Final
		if row.UpdatedAt.After(t.last) {
			t.last = row.UpdatedAt
		}
		if row.TokenExpiresAt.After(t.expires) {
			t.expires = row.TokenExpiresAt
		}
	}
	var claimed []Row
	for _, t := range turns {
		settled := t.allFinal && now.Sub(t.last) >= settle
		expired := !t.expires.IsZero() && now.Sub(t.expires) >= maxHold
		if !settled && !expired {
			continue
		}
		for _, row := range t.rows {
			row.audited = true
			claimed = append(claimed, row.Row)
		}
	}
	return claimed, nil
}

// DeleteOlderThan implements Store.
func (s *MemoryStore) DeleteOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	for key, row := range s.rows {
		if row.UpdatedAt.Before(cutoff) {
			delete(s.rows, key)
			deleted++
		}
	}
	return deleted, nil
}

// MarkFinal implements Store.
func (s *MemoryStore) MarkFinal(_ context.Context, executionID, replica string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[rowKey{executionID, replica}]; ok && !row.audited {
		row.Final, row.UpdatedAt = true, now
	}
	return nil
}

// Rows is the number of rows held, claimed or not, for tests.
func (s *MemoryStore) Rows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}
