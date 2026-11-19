package microsoftapps

import (
	"strings"
	"testing"
)

func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := env[name]; return value, ok }
}

func TestSeedSecretEnvName(t *testing.T) {
	for key, want := range map[string]string{
		"corp":      "ELITEA_MICROSOFT_OAUTH_APP_SECRET_CORP",
		"corp-prod": "ELITEA_MICROSOFT_OAUTH_APP_SECRET_CORP_PROD",
		"a1-b2-c3":  "ELITEA_MICROSOFT_OAUTH_APP_SECRET_A1_B2_C3",
	} {
		if got := SeedSecretEnv(key); got != want {
			t.Errorf("SeedSecretEnv(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestSeedFromEnv(t *testing.T) {
	if apps, err := SeedFromEnv(lookupOf(nil)); err != nil || apps != nil {
		t.Errorf("unset must seed nothing: %v %v", apps, err)
	}
	for _, empty := range []string{"", "  ", "[]", "null"} {
		if apps, err := SeedFromEnv(lookupOf(map[string]string{SeedConfigEnv: empty})); err != nil || apps != nil {
			t.Errorf("%q must seed nothing: %v %v", empty, apps, err)
		}
	}
	env := map[string]string{
		SeedConfigEnv: `[{"key":"corp","name":"Corp Microsoft","clientId":"` + testClientID + `",
			"authority":"https://login.microsoftonline.com/organizations","credentialTypes":["outlook","teams"],
			"scopeCeiling":{"outlook":["Mail.Read","Mail.Send"]},"autoRefreshToken":false,"secretExpiresAt":"2027-06-30"}]`,
		"ELITEA_MICROSOFT_OAUTH_APP_SECRET_CORP": "  " + plaintextSecret + "\n",
	}
	apps, err := SeedFromEnv(lookupOf(env))
	if err != nil || len(apps) != 1 {
		t.Fatalf("%v %v", apps, err)
	}
	if apps[0].Secret != plaintextSecret {
		t.Errorf("secret = %q, want the trimmed Secret value", apps[0].Secret)
	}
	in, err := apps[0].input()
	if err != nil || in.AutoRefreshToken || in.SecretExpiresAt == nil || in.SecretExpiresAt.Year() != 2027 {
		t.Errorf("input = %+v %v", in, err)
	}
	// The JSON never carries the secret: the field is not decoded from it.
	leak := map[string]string{SeedConfigEnv: `[{"key":"corp","name":"x","Secret":"` + plaintextSecret + `"}]`}
	if _, err := SeedFromEnv(lookupOf(leak)); err == nil {
		t.Error("a secret in the JSON was accepted")
	}
}

func TestSeedFromEnvRefusesMalformedEntries(t *testing.T) {
	good := `"name":"Corp","clientId":"` + testClientID + `","authority":"https://login.microsoftonline.com/organizations","credentialTypes":["outlook"]`
	for name, doc := range map[string]string{
		"not json":      `[`,
		"unknown field": `[{"key":"corp",` + good + `,"extra":1}]`,
		"bad key":       `[{"key":"Corp Inc",` + good + `}]`,
		"duplicate":     `[{"key":"corp",` + good + `},{"key":"corp",` + good + `}]`,
		"bad client":    `[{"key":"corp","name":"x","clientId":"nope","authority":"https://login.microsoftonline.com/common","credentialTypes":["outlook"]}]`,
		"foreign host":  `[{"key":"corp","name":"x","clientId":"` + testClientID + `","authority":"https://evil.example/common","credentialTypes":["outlook"]}]`,
		"bad date":      `[{"key":"corp",` + good + `,"secretExpiresAt":"soon"}]`,
	} {
		if _, err := SeedFromEnv(lookupOf(map[string]string{SeedConfigEnv: doc})); err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), plaintextSecret) {
			t.Errorf("%s: the error carries a secret", name)
		}
	}
}
