package desktopops

import (
	"context"
	"sync"
)

// memoryReplays is the handler tests' PlatformReplayStore: the semantics of
// *NATSPlatformReplays (claim, answer, release at a revision) in one process.
// It is a test double only; production composes the KV store, because a
// per-replica store lets a retry on another replica run a write twice.
type memoryReplays struct {
	mu       sync.Mutex
	entries  map[string]memoryReplay
	revision uint64
	err      error
}

type memoryReplay struct {
	revision uint64
	digest   string
	done     bool
	result   PlatformReplayResult
}

func newMemoryReplays() *memoryReplays { return &memoryReplays{entries: map[string]memoryReplay{}} }

func (m *memoryReplays) Claim(_ context.Context, key, digest string) (PlatformReplay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return PlatformReplay{}, m.err
	}
	if held, ok := m.entries[key]; ok {
		switch {
		case held.digest != digest:
			return PlatformReplay{Verdict: ReplayConflict}, nil
		case !held.done:
			return PlatformReplay{Verdict: ReplayInProgress}, nil
		default:
			return PlatformReplay{Verdict: ReplayDone, Result: held.result}, nil
		}
	}
	m.revision++
	m.entries[key] = memoryReplay{revision: m.revision, digest: digest}
	return PlatformReplay{Verdict: ReplayClaimed, Revision: m.revision, Digest: digest}, nil
}

func (m *memoryReplays) Complete(_ context.Context, key string, claim PlatformReplay, result PlatformReplayResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if held, ok := m.entries[key]; ok && held.revision == claim.Revision {
		if len(result.Body) > platformReplayMaxBody {
			result.Body, result.Omitted = nil, true
		}
		m.revision++
		m.entries[key] = memoryReplay{revision: m.revision, digest: held.digest, done: true, result: result}
	}
	return nil
}

func (m *memoryReplays) Release(_ context.Context, key string, claim PlatformReplay) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if held, ok := m.entries[key]; ok && held.revision == claim.Revision {
		delete(m.entries, key)
	}
	return nil
}
