package main

import (
	"errors"
	"strings"

	"github.com/elitea-ng/elitea-platform/libs/go/secretstrength"
)

// requireStrongSigningSecrets refuses to start elitea-main when a signing
// secret it holds is weak (#2958): blank, shorter than
// secretstrength.MinBytes, a well-known placeholder, or a value that was
// published in the source repository. It reads the environment through the
// injected getenv and has no side effects, so the policy is testable without
// executing start-up.
//
// It runs before anything derives a key from these values. APPLICATION_SECRET_KEY
// is the root of the run-token, vector-token and publish-validation keys, and
// a derived key must never exist from a weak root.
//
// The rules:
//
//   - APPLICATION_SECRET_KEY: optional (a deployment that never issues
//     personal access tokens is a real shape, and run() warns when it is
//     unset), but a value that is set must be strong;
//   - GATEWAY_IDENTITY_SECRET: optional here (the gateway requires it), and a
//     set value must be strong, because the gateway refuses a weak one;
//   - ELITEA_DEEPWIKI_IDENTITY_SECRET / ELITEA_INVENTORY_IDENTITY_SECRET:
//     REQUIRED when the matching facade is enabled, because the
//     sub-application host refuses every unsigned identity (#2959), and
//     strong whenever set.
//
// No error carries a value: secretstrength names the variable only.
func requireStrongSigningSecrets(getenv func(string) string) error {
	var errs []error
	for _, name := range []string{"APPLICATION_SECRET_KEY", "GATEWAY_IDENTITY_SECRET"} {
		if err := secretstrength.CheckIfSet(name, getenv(name)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, facade := range []struct{ enabled, secret string }{
		{"ELITEA_DEEPWIKI_ENABLED", "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"ELITEA_INVENTORY_ENABLED", "ELITEA_INVENTORY_IDENTITY_SECRET"},
	} {
		check := secretstrength.CheckIfSet
		if envTrue(getenv(facade.enabled)) {
			check = secretstrength.Check
		}
		if err := check(facade.secret, getenv(facade.secret)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// envTrue reads a boolean flag the way the facade configurations do.
func envTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
