// Package profile is the enterprise credential governance switch of
// ADR-0039: the boot flag, the immutable policy revisions and the profile
// pin every admission records.
//
// The flag is process configuration, read from the environment only. It is
// never read from the database, a request, a header or a user setting, so no
// caller can choose its own profile. With the flag off this package adds no
// database read or write to any request path (profile specification, flag-off
// contract items 3 and 6).
package profile

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// EnvEnabled turns the governed profile on. "true" or "false"; unset is
	// false; anything else refuses boot.
	EnvEnabled = "ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED"
	// EnvPolicyRevision names the active immutable policy revision. Required
	// when EnvEnabled is true, ignored otherwise.
	EnvPolicyRevision = "ELITEA_CREDENTIAL_GOVERNANCE_POLICY_REVISION"
)

// Profile is the credential profile an admission is pinned to.
type Profile string

const (
	ProfileLegacy   Profile = "elitea.credential-profile.legacy.v1"
	ProfileGoverned Profile = "elitea.credential-profile.governed.v1"
)

// ErrInvalidConfig is the typed boot refusal for a malformed flag or
// revision id. Its wrapped text names the variable, never a secret.
var ErrInvalidConfig = errors.New("credential governance configuration is invalid")

// revisionIDPattern bounds an operator-chosen revision id ("2026-10-r1").
var revisionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Config is the validated environment switch.
type Config struct {
	Enabled bool
	// ActivePolicyRevisionID names an immutable revision when Enabled. Its
	// existence and digest are checked against the database at boot (Boot).
	ActivePolicyRevisionID string
}

// ConfigFromEnv reads the switch. Values are trimmed and then compared
// case-sensitively: "true" and "false" only, so " TRUE " and "1" refuse boot
// rather than silently choosing a profile.
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("credential governance environment lookup is required")
	}
	raw, ok := lookup(EnvEnabled)
	value := strings.TrimSpace(raw)
	var cfg Config
	switch {
	case !ok || value == "" || value == "false":
	case value == "true":
		cfg.Enabled = true
	default:
		return Config{}, fmt.Errorf("%w: %s=%q: want \"true\" or \"false\"", ErrInvalidConfig, EnvEnabled, raw)
	}
	if cfg.Enabled {
		revision, _ := lookup(EnvPolicyRevision)
		cfg.ActivePolicyRevisionID = strings.TrimSpace(revision)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the shape of the configuration only.
func (c Config) Validate() error {
	if !c.Enabled {
		// The revision variable is ignored while the flag is off, so turning
		// the flag off needs no second edit.
		return nil
	}
	if c.ActivePolicyRevisionID == "" {
		return fmt.Errorf("%w: %s=true requires %s", ErrInvalidConfig, EnvEnabled, EnvPolicyRevision)
	}
	if !ValidRevisionID(c.ActivePolicyRevisionID) {
		return fmt.Errorf("%w: %s must match %s", ErrInvalidConfig, EnvPolicyRevision, revisionIDPattern.String())
	}
	return nil
}

// ValidRevisionID reports whether id is an acceptable revision id.
func ValidRevisionID(id string) bool {
	return revisionIDPattern.MatchString(id)
}
