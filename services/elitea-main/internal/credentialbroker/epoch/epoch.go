// Package epoch reads and checks the revocation epochs of governed credential
// use (ADR-0039 decisions 7.8 and 10, issue #1121).
//
// Every governed authority record carries an epoch that its transition bumps
// by exactly one: the agent identity binding, the mandate, the delegation,
// the credential use grant, the credential reference (its generation) and
// the external agent identity. A run's stored security context and a
// credential decision name the epochs they were taken at. An enforcement
// point (ClaimCommand, RenewLease, RedeemCredential, a cache hit) asks the
// Checker whether those epochs are still current; PostgreSQL is the
// authority and answers in ONE statement. A key whose row is missing or not
// active is stale whatever its epoch.
//
// The Hub fans an epoch change out to this replica's listeners (the
// credential cache) and, through a Transport, to every other replica. Fan-out
// is reference-only (kind, id, new epoch, change time) and advisory: a lost
// message costs latency only, because every cache hit re-checks the epochs.
package epoch

import (
	"context"
	"slices"

	"github.com/google/uuid"
)

// Kind names the record an epoch belongs to.
type Kind string

// The record kinds. Add a kind; never rename one: the name travels in the
// invalidation message.
const (
	KindBinding          Kind = "binding"
	KindMandate          Kind = "mandate"
	KindDelegation       Kind = "delegation"
	KindGrant            Kind = "grant"
	KindCredentialRef    Kind = "credential_ref"
	KindExternalIdentity Kind = "external_identity"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindBinding, KindMandate, KindDelegation, KindGrant, KindCredentialRef, KindExternalIdentity:
		return true
	}
	return false
}

// Unpinned is a key Value that asks only "is the record active?": any
// current epoch of an active record matches it. A read answers the current
// epoch, which a cache entry then stores.
const Unpinned int64 = -1

// Revoked is the Value a read answers for a key whose row is missing or not
// active. No stored epoch ever equals it.
const Revoked int64 = -2

// SecurityEpoch is one record's epoch.
type SecurityEpoch struct {
	Kind  Kind
	ID    string
	Value int64
}

// WellFormed reports whether e names a known kind and a UUID id.
func (e SecurityEpoch) WellFormed() bool {
	if !e.Kind.Valid() || e.Value < Unpinned {
		return false
	}
	id, err := uuid.Parse(e.ID)
	return err == nil && id.String() == e.ID
}

// Checker answers which of keys are stale: the record is missing, not
// active, or at another epoch than the key's (an Unpinned key is stale only
// when the record is missing or not active). A read failure is an error,
// never an empty answer.
type Checker interface {
	Current(ctx context.Context, keys []SecurityEpoch) (stale []SecurityEpoch, err error)
}

// Reader answers the current epoch of every key, in key order: Revoked for a
// missing or inactive record.
type Reader interface {
	Read(ctx context.Context, keys []SecurityEpoch) ([]SecurityEpoch, error)
}

// Stale compares keys with the current values a Reader answered (same
// order) and returns the stale keys.
func Stale(keys, current []SecurityEpoch) []SecurityEpoch {
	var stale []SecurityEpoch
	for i, key := range keys {
		if i >= len(current) || current[i].Kind != key.Kind || current[i].ID != key.ID {
			stale = append(stale, key)
			continue
		}
		value := current[i].Value
		if value < 0 || (key.Value != Unpinned && value != key.Value) {
			stale = append(stale, key)
		}
	}
	return stale
}

// Equal reports whether a and b hold the same epochs, in any order.
func Equal(a, b []SecurityEpoch) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := Sorted(a), Sorted(b)
	return slices.Equal(sa, sb)
}

// Sorted returns a sorted copy of keys.
func Sorted(keys []SecurityEpoch) []SecurityEpoch {
	out := slices.Clone(keys)
	slices.SortFunc(out, func(x, y SecurityEpoch) int {
		switch {
		case x.Kind != y.Kind:
			if x.Kind < y.Kind {
				return -1
			}
			return 1
		case x.ID != y.ID:
			if x.ID < y.ID {
				return -1
			}
			return 1
		case x.Value < y.Value:
			return -1
		case x.Value > y.Value:
			return 1
		}
		return 0
	})
	return out
}

// Contains reports whether keys names the record of e (kind and id) at an
// epoch below e's: an entry filled before the change e announces.
func Contains(keys []SecurityEpoch, e SecurityEpoch) bool {
	for _, key := range keys {
		if key.Kind == e.Kind && key.ID == e.ID && (e.Value < 0 || key.Value < e.Value) {
			return true
		}
	}
	return false
}
