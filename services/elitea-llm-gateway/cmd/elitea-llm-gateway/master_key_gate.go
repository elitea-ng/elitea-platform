package main

import (
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// requireVaultMasterKey decides whether the gateway may start with the master
// key it was given. It reads the environment through the injected getenv and
// has no side effects, so the policy is testable without executing start-up.
//
// It is the gateway half of elitea-main's gate (services/elitea-main/cmd/
// elitea-main/master_key_gate.go), with the same variables and the same rules:
//
//   - a valid key: start, nothing to report;
//   - a malformed key: refuse, whatever the opt-out says — a typo must never
//     downgrade wrapped reads to plaintext;
//   - no key: refuse, unless account.AllowUnwrappedEnvVar is "true". That
//     opt-out is for a developer machine and returns a warning the caller logs.
//
// It runs whether or not a database pool exists: the posture belongs to the
// deployment, and a pool that answers later must not find a keyless vault.
// No error here carries the key value: account.MasterKeyFromEnv reports only
// the variable name and the decode fault.
func requireVaultMasterKey(getenv func(string) string) (warning string, err error) {
	key, err := account.MasterKeyFromEnv(getenv)
	if err != nil {
		return "", err
	}
	optOut := false
	switch getenv(account.AllowUnwrappedEnvVar) {
	case "", "false":
	case "true":
		optOut = true
	default:
		return "", fmt.Errorf("%s must be true or false", account.AllowUnwrappedEnvVar)
	}
	if key != nil {
		return "", nil
	}
	if !optOut {
		return "", fmt.Errorf("%s is required: the gateway opens every project's provider credentials through "+
			"project vault keys wrapped with it, and refuses to read them in the clear. Supply a base64url-encoded "+
			"32-byte Fernet key (the same value elitea-main uses). For a throwaway local stack only, set %s=true "+
			"to read the keys unwrapped",
			account.MasterKeyEnvVar, account.AllowUnwrappedEnvVar)
	}
	return fmt.Sprintf("%s=true and no %s: every project vault key is read UNWRAPPED, "+
		"so anyone who can read the database can open every project's provider credentials. Development use only",
		account.AllowUnwrappedEnvVar, account.MasterKeyEnvVar), nil
}
