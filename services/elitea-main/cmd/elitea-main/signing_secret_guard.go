package main

import (
	"errors"
	"strconv"
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
//   - GATEWAY_IDENTITY_SECRET: REQUIRED when LLM_GATEWAY_URL is set, because
//     the gateway refuses every unsigned identity (#2959) and every /llm call
//     would then fail with 403; otherwise optional, and strong whenever set;
//   - ELITEA_DEEPWIKI_IDENTITY_SECRET / ELITEA_INVENTORY_IDENTITY_SECRET:
//     REQUIRED when the matching facade is enabled, because the
//     sub-application host refuses every unsigned identity (#2959), and
//     strong whenever set.
//
// No error carries a value: secretstrength names the variable only.
func requireStrongSigningSecrets(getenv func(string) string) error {
	var errs []error
	if err := secretstrength.CheckIfSet("APPLICATION_SECRET_KEY", getenv("APPLICATION_SECRET_KEY")); err != nil {
		errs = append(errs, err)
	}
	gatewayCheck := secretstrength.CheckIfSet
	if strings.TrimSpace(getenv("LLM_GATEWAY_URL")) != "" {
		gatewayCheck = secretstrength.Check
	}
	if err := gatewayCheck("GATEWAY_IDENTITY_SECRET", getenv("GATEWAY_IDENTITY_SECRET")); err != nil {
		errs = append(errs, err)
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

// envTrue reads a boolean flag. It accepts every true form of
// strconv.ParseBool (the facades parse their flag with it: 1, t, T, true,
// TRUE, True) and also yes and on, so the guard never reads an enabled
// facade as disabled.
func envTrue(raw string) bool {
	value := strings.TrimSpace(raw)
	if b, err := strconv.ParseBool(value); err == nil {
		return b
	}
	switch strings.ToLower(value) {
	case "yes", "on":
		return true
	}
	return false
}
