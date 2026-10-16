package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

type encryptionStub struct {
	storage.ObjectStore
	confirmed bool
	err       error
}

func (s encryptionStub) DefaultEncryptionConfirmed(context.Context) (bool, error) {
	return s.confirmed, s.err
}

// The attestation applies ONLY to a backend that cannot report its
// encryption. A backend that reports no rule, or fails, is refused whatever
// the operator says, and the default is a refusal.
func TestCheckWorkspaceEncryption(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	unreportable := fmt.Errorf("s3: %w", storage.ErrEncryptionUnreportable)
	type plain struct{ storage.ObjectStore }
	for _, tc := range []struct {
		name     string
		store    storage.ObjectStore
		attested bool
		want     string
	}{
		{"a reported rule", encryptionStub{confirmed: true}, false, workspaceEncryptionConfirmed},
		{"a reported rule, attested too", encryptionStub{confirmed: true}, true, workspaceEncryptionConfirmed},
		{"no rule", encryptionStub{}, false, workspaceEncryptionRefused},
		{"no rule, attested", encryptionStub{}, true, workspaceEncryptionRefused},
		{"an error, attested", encryptionStub{err: fmt.Errorf("access denied")}, true, workspaceEncryptionRefused},
		{"cannot report", encryptionStub{err: unreportable}, false, workspaceEncryptionRefused},
		{"cannot report, attested", encryptionStub{err: unreportable}, true, workspaceEncryptionAttested},
		{"no check at all, attested", plain{}, true, workspaceEncryptionAttested},
		{"through the instrumented wrapper", storage.Instrument(encryptionStub{err: unreportable}, "s3"), true, workspaceEncryptionAttested},
	} {
		if got := checkWorkspaceEncryption(context.Background(), tc.store, tc.attested, logger, "default"); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct{ a, b, want string }{
		{workspaceEncryptionConfirmed, workspaceEncryptionConfirmed, workspaceEncryptionConfirmed},
		{workspaceEncryptionConfirmed, workspaceEncryptionAttested, workspaceEncryptionAttested},
		{workspaceEncryptionAttested, workspaceEncryptionRefused, workspaceEncryptionRefused},
		{workspaceEncryptionRefused, workspaceEncryptionConfirmed, workspaceEncryptionRefused},
	} {
		if got := combineWorkspaceEncryption(tc.a, tc.b); got != tc.want {
			t.Fatalf("combine(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestWorkspaceStorageFromEnvReadsTheAttestation(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	}
	inputs, err := workspaceStorageFromEnv(context.Background(), storage.Config{Container: "elitea"}, lookup(map[string]string{}))
	if err != nil || inputs.EncryptionAttested {
		t.Fatalf("unset = %+v, %v; want not attested (the default refuses)", inputs, err)
	}
	inputs, err = workspaceStorageFromEnv(context.Background(), storage.Config{Container: "elitea"},
		lookup(map[string]string{workspaceEncryptionAttestedEnv: "true"}))
	if err != nil || !inputs.EncryptionAttested {
		t.Fatalf("true = %+v, %v; want attested", inputs, err)
	}
	if _, err := workspaceStorageFromEnv(context.Background(), storage.Config{Container: "elitea"},
		lookup(map[string]string{workspaceEncryptionAttestedEnv: "yes please"})); err == nil ||
		!strings.Contains(err.Error(), workspaceEncryptionAttestedEnv) {
		t.Fatalf("an invalid value = %v, want a startup error naming the variable", err)
	}
}
