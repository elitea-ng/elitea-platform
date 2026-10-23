package proxy

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
)

// KeySource holds the current run-token key set.
type KeySource interface {
	Current() *runtoken.KeySet
}

// StaticKeys is a fixed key set, for tests.
type StaticKeys struct{ Set *runtoken.KeySet }

// Current returns the set.
func (s StaticKeys) Current() *runtoken.KeySet { return s.Set }

// FileKeys reads the key set from a file or directory (a mounted Secret) and
// reads it again on an interval, so a key rotation needs no restart. A reload
// that fails keeps the last good set and counts an error: a half-written
// Secret update must not stop every run.
type FileKeys struct {
	path    string
	current atomic.Pointer[runtoken.KeySet]
	logger  *slog.Logger
	metrics *Metrics
}

// LoadFileKeys reads the key set once. A failure here is a startup error.
func LoadFileKeys(path string, logger *slog.Logger, metrics *Metrics) (*FileKeys, error) {
	ks, err := runtoken.LoadKeySet(path)
	if err != nil {
		return nil, err
	}
	f := &FileKeys{path: path, logger: logger, metrics: metrics}
	f.current.Store(ks)
	return f, nil
}

// Current returns the last good set.
func (f *FileKeys) Current() *runtoken.KeySet { return f.current.Load() }

// Reload reads the set again. It logs a change of key ids.
func (f *FileKeys) Reload() {
	ks, err := runtoken.LoadKeySet(f.path)
	if err != nil {
		f.metrics.KeyReloadFailed()
		f.logger.Warn("egress_proxy.key_reload_failed", "error", err.Error())
		return
	}
	old := f.current.Swap(ks)
	before, after := old.KeyIDs(), ks.KeyIDs()
	slices.Sort(before)
	slices.Sort(after)
	if !slices.Equal(before, after) {
		f.logger.Info("egress_proxy.keys_changed", "key_ids", after, "previous_key_ids", before)
	}
}

// Run reloads on every interval until ctx ends. A zero interval returns.
func (f *FileKeys) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Reload()
		}
	}
}
