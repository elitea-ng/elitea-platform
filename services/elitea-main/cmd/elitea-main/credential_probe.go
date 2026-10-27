package main

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	v2credentialprobe "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialprobe"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// credentialProbeAdmin builds the credential probe handler (#1120). Nil
// with the flag off, so the router mounts no probe route. With the flag on
// it reads references and grants from the platform database, audits every
// probe, and answers from the built-in adapter registry. No provider probe
// runs yet: the prober is the fail-closed default, which answers
// "unavailable" without reading any material.
func credentialProbeAdmin(admission profile.Admission, pool *pgxpool.Pool) (*v2credentialprobe.Handler, error) {
	if !admission.Governed() {
		return nil, nil
	}
	if pool == nil {
		return nil, errors.New("the credential probe needs the platform database")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, fmt.Errorf("construct credential probe security audit appender: %w", err)
	}
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		return nil, fmt.Errorf("construct the credential adapter registry: %w", err)
	}
	store := repos.NewCredentialGrants(pool, audit)
	return v2credentialprobe.New(v2credentialprobe.Config{
		References:  store,
		Grants:      store,
		Audit:       store,
		Descriptors: registry,
		Prober:      credentialadapters.UnavailableProber{},
	})
}
