package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
)

type entraProviderSource struct {
	t        *testing.T
	provider identityproviders.Provider
	err      error
	calls    int
	forbid   bool
}

func (s *entraProviderSource) Enabled(context.Context, identityproviders.Kind) (identityproviders.Provider, error) {
	s.calls++
	if s.forbid {
		s.t.Fatal("flag off: the identity provider must not be read for Entra governance")
	}
	return s.provider, s.err
}

func forbiddenMasterKey(t *testing.T) func() ([]byte, error) {
	return func() ([]byte, error) {
		t.Fatal("the master key must not be read")
		return nil, nil
	}
}

// Flag off: nothing is read or built, whatever the provider says.
func TestEntraGovernanceIsNotBuiltWithTheFlagOff(t *testing.T) {
	source := &entraProviderSource{t: t, forbid: true}
	got, err := entraGovernanceAtBoot(context.Background(), profile.LegacyAdmission(false), nil, source,
		forbiddenMasterKey(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || got != nil || source.calls != 0 {
		t.Fatalf("got=%v err=%v calls=%d", got, err, source.calls)
	}
}

// Governed, but the provider is not an Entra provider (or there is none):
// nothing is built and no key is derived.
func TestEntraGovernanceNeedsAnEntraProvider(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "r1"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, source := range map[string]*entraProviderSource{
		"none":      {t: t, err: identityproviders.ErrNotFound},
		"non-entra": {t: t, provider: identityproviders.Provider{Key: "okta", OIDC: &identityproviders.OIDCDocument{}}},
		"saml":      {t: t, provider: identityproviders.Provider{Key: "saml"}},
	} {
		got, err := entraGovernanceAtBoot(context.Background(), governed, nil, source, forbiddenMasterKey(t), nil, logger)
		if err != nil || got != nil {
			t.Fatalf("%s: got=%v err=%v", name, got, err)
		}
	}
	_, err := entraGovernanceAtBoot(context.Background(), governed, nil,
		&entraProviderSource{t: t, err: errors.New("database down")}, forbiddenMasterKey(t), nil, logger)
	if err == nil {
		t.Fatal("an unreadable provider table must refuse a governed boot")
	}
}
