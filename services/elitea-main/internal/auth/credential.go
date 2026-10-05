package auth

import (
	"errors"
	"fmt"
)

var (
	// ErrCredentialRejected is an ordinary authentication result: the supplied
	// credential is malformed, expired, revoked, or unknown.
	ErrCredentialRejected = errors.New("authentication credential rejected")
	// ErrCredentialValidationUnavailable identifies configuration, storage, or
	// integrity failures while checking a credential. Callers must fail closed
	// without converting this outage into anonymous or public access.
	ErrCredentialValidationUnavailable = errors.New("authentication credential validation unavailable")
	// ErrDeviceRevoked is a NATIVE credential whose device session (refresh-
	// token family) is revoked, expired, or belongs to a deactivated or deleted
	// user (ADR-0025 decision 4). It WRAPS ErrCredentialRejected, so every
	// caller that only knows "rejected" keeps treating it as a refusal; a caller
	// that recognises it answers `{"error":"device_revoked"}`, which a native
	// client reads as "wipe local data" rather than "refresh once".
	//
	// Only a validator that recognised a native credential may return it.
	ErrDeviceRevoked = fmt.Errorf("%w: device session revoked", ErrCredentialRejected)
)
