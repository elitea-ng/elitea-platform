package facade_test

import (
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

func TestAdmissionPostureDefaultsToRecordAndRefusesATypo(t *testing.T) {
	env := func(value string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name != facade.AdmissionPostureEnv {
				return "", false
			}
			return value, true
		}
	}
	for name, c := range map[string]struct {
		lookup func(string) (string, bool)
		want   facade.AdmissionPosture
	}{
		// Unset is record, and so is a variable somebody blanked: nothing in
		// this deployment can reach `active`, so enforcing by default would
		// refuse every provider on every install.
		"unset":        {nil, facade.AdmissionRecord},
		"empty":        {env(""), facade.AdmissionRecord},
		"whitespace":   {env("  "), facade.AdmissionRecord},
		"record":       {env("record"), facade.AdmissionRecord},
		"enforce":      {env("enforce"), facade.AdmissionEnforce},
		"ENFORCE":      {env("ENFORCE"), facade.AdmissionEnforce},
		"padded":       {env(" enforce\n"), facade.AdmissionEnforce},
		"mixed record": {env("Record"), facade.AdmissionRecord},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := c.lookup
			if lookup == nil {
				// A nil lookup falls back to the process environment, which
				// this test does not set: the same "unset" answer.
				lookup = func(string) (string, bool) { return "", false }
			}
			got, err := facade.AdmissionPostureFromEnv(lookup)
			if err != nil {
				t.Fatalf("%q was refused: %v", name, err)
			}
			if got != c.want {
				t.Fatalf("%q read as %q, want %q", name, got, c.want)
			}
		})
	}

	// The whole point of the parse: a typo must NOT read as the default. An
	// operator who believes they are enforcing and is silently recording is
	// the failure this rules out.
	for _, typo := range []string{"enfroce", "records", "true", "on", "off", "1"} {
		got, err := facade.AdmissionPostureFromEnv(env(typo))
		if !errors.Is(err, facade.ErrIncompleteConfig) {
			t.Errorf("%q was accepted as %q (err %v)", typo, got, err)
		}
		if got != "" {
			t.Errorf("%q was refused AND returned a posture %q", typo, got)
		}
	}
}
