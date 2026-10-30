// Package credentialuse records when a credential an agent can use was last
// used (ADR-0044 decision 5, #1501): a personal access token authenticating
// a request, a toolkit secret resolved for a run, an MCP OAuth token or a
// provider OAuth grant loaded for a toolkit, and an ADR-0039 credential
// reference and use grant redeemed by the credential broker.
//
// # Coalescing: one write per credential per hour, never on the request
//
// The PAT path authenticates every API request, so a statement per use is
// out of the question. The design reuses the SCIM client pattern
// (scimclient.Store.TouchLastUsed: an in-process map plus an SQL guard) and
// moves the write off the request:
//
//   - Touch only takes a mutex and reads a map. A credential already marked
//     in the last hour is dropped there, with no statement at all. Otherwise
//     the use is marked and queued.
//   - Run flushes the queue every FlushInterval (30 s) in ONE batched upsert
//     on elitea_identity.credential_last_use. The upsert's WHERE keeps the
//     stored value when it is less than an hour older than the new one, so
//     several replicas between them still write a credential at most about
//     once an hour (each replica marks on its own; the guard drops the
//     second replica's write).
//   - The queue is bounded (MaxPending). A use beyond it is dropped and
//     un-marked so a later use queues it again; last use is evidence, not
//     an authorization input, so losing one in an overload costs nothing
//     but an hour of precision.
//
// last_used_at is therefore accurate to the hour (and to the flush interval
// on shutdown), which is all a 90-day staleness threshold and a 30-day
// trimming window need.
package credentialuse

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Kind is the kind of a credential.
type Kind string

// The kinds the inventory lists. The column's CHECK in
// migrations/shared/0208_credential_inventory.sql names the same set.
const (
	KindPAT                 Kind = "pat"
	KindToolkitSecret       Kind = "toolkit_secret"
	KindMCPOAuthToken       Kind = "mcp_oauth_token"
	KindProviderOAuthGrant  Kind = "provider_oauth_grant"
	KindCredentialReference Kind = "credential_reference"
	KindCredentialGrant     Kind = "credential_grant"
)

// Kinds lists every kind, in the inventory's order.
var Kinds = []Kind{
	KindPAT, KindToolkitSecret, KindMCPOAuthToken, KindProviderOAuthGrant,
	KindCredentialReference, KindCredentialGrant,
}

// Valid reports whether the kind is known.
func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// Coalesce is the shortest interval between two recorded uses of one
// credential.
const Coalesce = time.Hour

// FlushInterval is how often Run writes the queued uses.
const FlushInterval = 30 * time.Second

// MaxPending bounds the queue between two flushes.
const MaxPending = 10_000

// maxMarks bounds the map of marked credentials; Run prunes marks older
// than Coalesce at every flush, so it only fills under a flood of distinct
// credentials, and is then reset.
const maxMarks = 200_000

// Use is one queued use.
type Use struct {
	Kind         Kind
	CredentialID string
	// ProjectID is the credential's project when the caller knows it, else 0.
	ProjectID int64
	At        time.Time
}

// Store writes queued uses. It returns how many rows it changed.
type Store interface {
	WriteLastUse(ctx context.Context, uses []Use) (int64, error)
}

type key struct {
	kind Kind
	id   string
}

// Recorder is the coalescer. The zero value is not usable; use New. A nil
// *Recorder ignores every call.
type Recorder struct {
	store  Store
	now    func() time.Time
	logger *slog.Logger

	mu      sync.Mutex
	marks   map[key]time.Time
	pending map[key]Use
	written atomic.Int64
}

// New builds a recorder over the store.
func New(store Store, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{
		store: store, now: time.Now, logger: logger,
		marks: map[key]time.Time{}, pending: map[key]Use{},
	}
}

// WithClock replaces the clock (tests).
func (r *Recorder) WithClock(now func() time.Time) *Recorder {
	r.now = now
	return r
}

// Touch records one use. It never blocks on the database.
func (r *Recorder) Touch(kind Kind, credentialID string, projectID int64) {
	if r == nil || credentialID == "" || len(credentialID) > 256 {
		return
	}
	now := r.now()
	k := key{kind: kind, id: credentialID}
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.marks[k]; ok && now.Sub(last) < Coalesce {
		return
	}
	if len(r.pending) >= MaxPending {
		return
	}
	if len(r.marks) >= maxMarks {
		r.marks = map[key]time.Time{}
	}
	r.marks[k] = now
	r.pending[k] = Use{Kind: kind, CredentialID: credentialID, ProjectID: projectID, At: now.UTC()}
}

// TouchID records one use of a credential with a numeric id.
func (r *Recorder) TouchID(kind Kind, id int64, projectID int64) {
	if id <= 0 {
		return
	}
	r.Touch(kind, strconv.FormatInt(id, 10), projectID)
}

// Flush writes the queued uses in one statement. A failed write re-queues
// nothing: the marks are dropped so the next use of each credential queues
// it again.
func (r *Recorder) Flush(ctx context.Context) (int64, error) {
	if r == nil || r.store == nil {
		return 0, nil
	}
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.pruneLocked()
		r.mu.Unlock()
		return 0, nil
	}
	batch := make([]Use, 0, len(r.pending))
	for _, use := range r.pending {
		batch = append(batch, use)
	}
	r.pending = map[key]Use{}
	r.pruneLocked()
	r.mu.Unlock()

	written, err := r.store.WriteLastUse(ctx, batch)
	if err != nil {
		r.mu.Lock()
		for _, use := range batch {
			delete(r.marks, key{kind: use.Kind, id: use.CredentialID})
		}
		r.mu.Unlock()
		return 0, err
	}
	r.written.Add(written)
	return written, nil
}

// Written is the number of rows the recorder's flushes changed (tests and
// diagnostics).
func (r *Recorder) Written() int64 {
	if r == nil {
		return 0
	}
	return r.written.Load()
}

func (r *Recorder) pruneLocked() {
	cutoff := r.now().Add(-Coalesce)
	for k, at := range r.marks {
		if at.Before(cutoff) {
			delete(r.marks, k)
		}
	}
}

// Run flushes every FlushInterval until ctx ends, then once more with a
// short deadline so a clean shutdown keeps the last uses.
func (r *Recorder) Run(ctx context.Context) {
	if r == nil {
		return
	}
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if _, err := r.Flush(final); err != nil {
				r.logger.Warn("credential last use: final flush failed", "err", err)
			}
			cancel()
			return
		case <-ticker.C:
			if _, err := r.Flush(ctx); err != nil {
				r.logger.Warn("credential last use: flush failed", "err", err)
			}
		}
	}
}

// The process-wide recorder. A personal access token is validated by two
// compositions (the authentication graph's PAT validator and main's session
// token validator, which the edge reuses), a toolkit secret by the claim
// materializer, and the broker records redemptions: one process-wide
// recorder, installed once at boot, reaches all of them without threading a
// field through each. Before Install (tests, tools) every Touch is a no-op.
var current atomic.Pointer[Recorder]

// Install sets the process-wide recorder.
func Install(r *Recorder) { current.Store(r) }

// Default is the process-wide recorder, nil before Install.
func Default() *Recorder { return current.Load() }
