package main

import (
	"fmt"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// requireVaultMasterKey decides whether elitea-main may start with the master
// key it was given. It reads the environment through the injected getenv and
// has no side effects, so the policy is testable without executing start-up.
//
// elitea-main always composes the secrets handler, which is the one writer of
// project vault keys, and that handler takes its key from SECRETS_MASTER_KEY
// alone (ELITEA_VAULT_MASTER_KEY_FILE only configures READERS, so it is not an
// equivalent source). The rules:
//
//   - a valid key: start, nothing to report;
//   - a malformed key: refuse, whatever the opt-out says — a typo must never
//     downgrade wrapped storage to plaintext (#412);
//   - no key: refuse, unless v2secrets.AllowUnwrappedEnvVar is "true". That
//     opt-out is for a developer machine and returns a warning the caller logs.
//
// No error here carries the key value: MasterKeyFromEnv reports only the
// variable name, a decode position and a length.
func requireVaultMasterKey(getenv func(string) string) (warning string, err error) {
	key, err := v2secrets.MasterKeyFromEnv(getenv)
	if err != nil {
		return "", err
	}
	optOut := false
	switch getenv(v2secrets.AllowUnwrappedEnvVar) {
	case "", "false":
	case "true":
		optOut = true
	default:
		return "", fmt.Errorf("%s must be true or false", v2secrets.AllowUnwrappedEnvVar)
	}
	if key != nil {
		return "", nil
	}
	if !optOut {
		return "", fmt.Errorf("%s is required: elitea-main stores every project vault key wrapped with it "+
			"and refuses to store them in the clear. Supply a base64url-encoded 32-byte Fernet key "+
			"(the same value the LLM gateway uses). For a throwaway local stack only, set %s=true "+
			"to store the keys unwrapped",
			v2secrets.MasterKeyEnvVar, v2secrets.AllowUnwrappedEnvVar)
	}
	return fmt.Sprintf("%s=true and no %s: every project vault key is stored UNWRAPPED, "+
		"so anyone who can read the database can open every project secret. Development use only",
		v2secrets.AllowUnwrappedEnvVar, v2secrets.MasterKeyEnvVar), nil
}
