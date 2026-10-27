package profile

import (
	"context"
	"errors"
	"fmt"
)

// ErrBootRefused wraps every reason the governed profile refuses boot.
var ErrBootRefused = errors.New("credential governance refused boot")

// Admission is the profile new admissions are pinned to. It is built once at
// boot and handed to every admission repository by value. The zero value is
// invalid, so a repository constructed without it fails rather than silently
// admitting legacy work while the flag is on.
type Admission struct {
	profile    Profile
	revisionID string
	digest     [32]byte
	// continuationGuard is set while governed pins may exist (the flag is on,
	// or it was on before this boot): a continuation then copies a governed
	// predecessor's pin instead of downgrading (spec item 5.2).
	continuationGuard bool
}

// LegacyAdmission is the flag-off admission: no pin rows are written.
// continuationGuard reports whether governed pins already exist (a flag-off
// boot after governed work); see Admission.ContinuationGuard.
func LegacyAdmission(continuationGuard bool) Admission {
	return Admission{profile: ProfileLegacy, continuationGuard: continuationGuard}
}

// GovernedAdmission pins new admissions to revision.
func GovernedAdmission(revision PolicyRevision) Admission {
	return Admission{profile: ProfileGoverned, revisionID: revision.ID, digest: revision.Digest, continuationGuard: true}
}

// Profile is the profile new admissions are pinned to.
func (a Admission) Profile() Profile { return a.profile }

// Governed reports whether new admissions write a governed pin.
func (a Admission) Governed() bool { return a.profile == ProfileGoverned }

// RevisionID is the pinned revision; empty for legacy.
func (a Admission) RevisionID() string { return a.revisionID }

// Digest is the pinned revision digest; zero for legacy.
func (a Admission) Digest() [32]byte { return a.digest }

// ContinuationGuard reports whether a continuation must look up its
// predecessor's pin. It is false on a deployment that never ran governed, so
// such a deployment adds no read to any admission (flag-off contract item 3).
func (a Admission) ContinuationGuard() bool { return a.continuationGuard }

// Validate refuses the zero value.
func (a Admission) Validate() error {
	switch a.profile {
	case ProfileLegacy:
		if a.revisionID != "" || a.digest != ([32]byte{}) {
			return errors.New("legacy credential admission carries a revision")
		}
		return nil
	case ProfileGoverned:
		if !ValidRevisionID(a.revisionID) || a.digest == ([32]byte{}) {
			return errors.New("governed credential admission requires a revision and digest")
		}
		return nil
	}
	return errors.New("credential admission profile is required")
}

// GovernedPinProbe reports whether any governed pin exists. Boot calls it
// once with the flag off to decide the continuation guard; it is never on a
// request path.
type GovernedPinProbe interface {
	AnyGovernedPin(ctx context.Context) (bool, error)
}

// Boot resolves the admission profile for this process.
//
// Flag off: no revision is read and no prerequisite runs. probe may be nil
// only in tests that do not need the guard; in production it is the pin
// store, and its one read happens here at boot.
//
// Flag on: the active revision must exist, hash to its digest, allow at
// least one identity mode (spec item 1.6) and satisfy every prerequisite
// (spec item 1.5). Any failure refuses boot; there is no partial governed
// mode.
func Boot(ctx context.Context, cfg Config, revisions RevisionStore, probe GovernedPinProbe, prerequisites []Prerequisite) (Admission, PolicyRevision, error) {
	if err := cfg.Validate(); err != nil {
		return Admission{}, PolicyRevision{}, err
	}
	if !cfg.Enabled {
		guard := false
		if probe != nil {
			found, err := probe.AnyGovernedPin(ctx)
			if err != nil {
				return Admission{}, PolicyRevision{}, fmt.Errorf("%w: probe governed pins: %w", ErrBootRefused, err)
			}
			guard = found
		}
		return LegacyAdmission(guard), PolicyRevision{}, nil
	}
	if revisions == nil {
		return Admission{}, PolicyRevision{}, fmt.Errorf("%w: revision store is required", ErrBootRefused)
	}
	revision, err := revisions.Revision(ctx, cfg.ActivePolicyRevisionID)
	if err != nil {
		return Admission{}, PolicyRevision{}, fmt.Errorf("%w: active revision %q: %w", ErrBootRefused, cfg.ActivePolicyRevisionID, err)
	}
	if err := ValidateForActivation(ctx, revision, prerequisites); err != nil {
		return Admission{}, PolicyRevision{}, fmt.Errorf("%w: %w", ErrBootRefused, err)
	}
	return GovernedAdmission(revision), revision, nil
}

// ValidateForActivation is the check boot runs on the active revision and the
// CLI runs before publishing a revision (spec item 1.7).
func ValidateForActivation(ctx context.Context, revision PolicyRevision, prerequisites []Prerequisite) error {
	if err := revision.VerifyDigest(); err != nil {
		return err
	}
	if err := revision.Document.Validate(); err != nil {
		return err
	}
	if len(revision.Document.AllowedIdentityModes) == 0 {
		return fmt.Errorf("%w: revision %q allows no identity mode, so every governed credential use would be denied", ErrInvalidPolicy, revision.ID)
	}
	for _, prerequisite := range prerequisites {
		if prerequisite == nil {
			return errors.New("credential governance prerequisite is nil")
		}
		if err := prerequisite.Check(ctx, revision); err != nil {
			return fmt.Errorf("prerequisite %s: %w", prerequisite.Name(), err)
		}
	}
	return nil
}
