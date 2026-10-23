package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type fakeStore struct {
	policies   []repos.AdmissionPolicy
	setCalls   int
	capability string
	max        int64
}

func (f *fakeStore) List(context.Context) ([]repos.AdmissionPolicy, error) { return f.policies, nil }

func (f *fakeStore) Set(_ context.Context, capabilityID string, maxOutstanding int64) (int64, error) {
	f.setCalls++
	f.capability, f.max = capabilityID, maxOutstanding
	var written int64
	for i := range f.policies {
		if capabilityID == "" || f.policies[i].CapabilityID == capabilityID {
			f.policies[i].MaxOutstanding = maxOutstanding
			written++
		}
	}
	return written, nil
}

func (f *fakeStore) Close() {}

func env(values map[string]string) lookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func runWith(t *testing.T, store *fakeStore, values map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, env(values), &stdout, &stderr,
		func(context.Context, string) (policyStore, error) { return store, nil })
	return code, stdout.String(), stderr.String()
}

func TestSetWithoutMaxTakesTheReplicasConfiguredCap(t *testing.T) {
	store := &fakeStore{policies: []repos.AdmissionPolicy{
		{CapabilityID: "agent.execute.application.v1", MaxOutstanding: 1024},
		{CapabilityID: "index.ingest.v1", MaxOutstanding: 1024},
	}}
	code, stdout, stderr := runWith(t, store,
		map[string]string{"DATABASE_URL": "postgres://db", "ELITEA_RUNTIME_MAX_OUTSTANDING": "2048"}, "set")
	if code != exitOK || stderr != "" {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if store.setCalls != 1 || store.capability != "" || store.max != 2048 {
		t.Fatalf("set call: %+v", store)
	}
	for _, want := range []string{"every persisted capability to 2048", "agent.execute.application.v1\t2048", "index.ingest.v1\t2048"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q: %s", want, stdout)
		}
	}
}

func TestSetOneCapabilityWithExplicitMax(t *testing.T) {
	store := &fakeStore{policies: []repos.AdmissionPolicy{{CapabilityID: "agent.execute.application.v1", MaxOutstanding: 64}}}
	code, _, stderr := runWith(t, store, map[string]string{"DATABASE_URL": "postgres://db"},
		"set", "-max", "4096", "-capability", "agent.execute.application.v1")
	if code != exitOK || store.capability != "agent.execute.application.v1" || store.max != 4096 {
		t.Fatalf("code=%d stderr=%s store=%+v", code, stderr, store)
	}
}

// -capability only changes an existing policy: an id with no row (here the
// pre-#966 example's agent.execute.v1, which no code admits under) writes
// nothing and exits non-zero.
func TestSetAnUnknownCapabilityFailsAndWritesNothing(t *testing.T) {
	store := &fakeStore{policies: []repos.AdmissionPolicy{{CapabilityID: "agent.execute.application.v1", MaxOutstanding: 64}}}
	code, stdout, stderr := runWith(t, store, map[string]string{"DATABASE_URL": "postgres://db"},
		"set", "-max", "128", "-capability", "agent.execute.v1")
	if code != exitFailed || !strings.Contains(stderr, `"agent.execute.v1" has no persisted admission cap`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "set the admission cap") || store.policies[0].MaxOutstanding != 64 {
		t.Fatalf("stdout=%q policies=%+v", stdout, store.policies)
	}
}

func TestSetRefusesCapsOutsideTheSupportedRangeWithoutTouchingTheDatabase(t *testing.T) {
	for _, args := range [][]string{
		{"set", "-max", "4097"},
		{"set", "-max", "-1"},
		{"set"}, // no -max and no ELITEA_RUNTIME_MAX_OUTSTANDING
		{"show", "-max", "5"},
		{"bogus"},
		{},
	} {
		store := &fakeStore{}
		code, _, _ := runWith(t, store, map[string]string{"DATABASE_URL": "postgres://db"}, args...)
		if code != exitInvalidUsage || store.setCalls != 0 {
			t.Fatalf("args %v: code=%d setCalls=%d", args, code, store.setCalls)
		}
	}
}

func TestShowListsPersistedCaps(t *testing.T) {
	store := &fakeStore{policies: []repos.AdmissionPolicy{{CapabilityID: "configuration.validate.v1", MaxOutstanding: 64}}}
	code, stdout, _ := runWith(t, store, map[string]string{"DATABASE_URL": "postgres://db"}, "show")
	if code != exitOK || store.setCalls != 0 || stdout != "configuration.validate.v1\t64\n" {
		t.Fatalf("code=%d setCalls=%d stdout=%q", code, store.setCalls, stdout)
	}
}
