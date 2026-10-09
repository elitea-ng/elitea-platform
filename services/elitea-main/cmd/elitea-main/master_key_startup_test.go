package main

// The start-up gate for SECRETS_MASTER_KEY (#412).
//
// run() must refuse to start on a malformed key, and it must refuse BEFORE it
// opens the database pool — so this case needs no PostgreSQL. That ordering is
// the point of the gate, not an accident of the test: the fault has to be
// caught before anything composes a handler, because two of the four
// NewHandler callers build one per request and would only fail long after
// provisioning had written vaults.

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

func TestRunRefusesToStartOnAMalformedMasterKey(t *testing.T) {
	// A valid key with a trailing SPACE. A trailing newline would not work:
	// Go's base64 decoder ignores "\r" and "\n", so such a key is still valid
	// and must keep working — see the secrets package's unit cases.
	valid := base64.URLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv(v2secrets.MasterKeyEnvVar, valid+" ")
	// The two variables run() reads before the gate. Cleared so a value in the
	// developer's own shell cannot stop run() earlier and fake a pass.
	t.Setenv("AUTH_DEV_MODE", "")
	t.Setenv("ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA", "")
	t.Setenv("ELITEA_HTTP_ADDRESS", "")
	// Unreachable on purpose. If the gate ever stops firing, run() reaches the
	// pool and this case fails on a DIFFERENT error rather than passing.
	t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/elitea?sslmode=disable&connect_timeout=1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatalf("run() started with a malformed %s; it must refuse, because the "+
			"secrets handler would otherwise store every project vault key unwrapped",
			v2secrets.MasterKeyEnvVar)
	}
	// Naming the variable is an acceptance criterion: the message is the only
	// thing the operator gets.
	if !strings.Contains(err.Error(), v2secrets.MasterKeyEnvVar) {
		t.Fatalf("run() failed with %q, which does not name %s", err, v2secrets.MasterKeyEnvVar)
	}
}

// MasterKeyFromEnv itself still reports an absent key as "no key" rather than
// as an error: the policy that an absent key stops the service lives in
// requireVaultMasterKey (master_key_gate.go), so the secrets package keeps one
// pure parser and the start-up decision stays in one place.
func TestMasterKeyFromEnvReportsAnAbsentKeyAsNoKey(t *testing.T) {
	key, err := v2secrets.MasterKeyFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("an absent %s must parse as no key, got %v", v2secrets.MasterKeyEnvVar, err)
	}
	if key != nil {
		t.Fatalf("an absent %s must yield no key, got %x", v2secrets.MasterKeyEnvVar, key)
	}
}

// run() must refuse an ABSENT key too, again before the database pool is
// opened. The unreachable DATABASE_URL turns a regressed gate into a different
// error instead of a silent pass.
func TestRunRefusesToStartWithoutAMasterKey(t *testing.T) {
	t.Setenv(v2secrets.MasterKeyEnvVar, "")
	t.Setenv(v2secrets.AllowUnwrappedEnvVar, "")
	t.Setenv("AUTH_DEV_MODE", "")
	t.Setenv("ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA", "")
	t.Setenv("ELITEA_HTTP_ADDRESS", "")
	t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/elitea?sslmode=disable&connect_timeout=1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatalf("run() started without %s; it must refuse, because the secrets handler "+
			"would otherwise store every project vault key unwrapped", v2secrets.MasterKeyEnvVar)
	}
	if !strings.Contains(err.Error(), v2secrets.MasterKeyEnvVar) {
		t.Fatalf("run() failed with %q, which does not name %s", err, v2secrets.MasterKeyEnvVar)
	}
	if !strings.Contains(err.Error(), v2secrets.AllowUnwrappedEnvVar) {
		t.Fatalf("run() failed with %q, which does not name the development opt-out %s",
			err, v2secrets.AllowUnwrappedEnvVar)
	}
}

func TestRequireVaultMasterKey(t *testing.T) {
	valid := base64.URLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	// 31 bytes: valid base64, wrong length. A distinctive value, so the
	// no-leak assertion below cannot pass by coincidence.
	shortKey := base64.URLEncoding.EncodeToString([]byte("SHORT-KEY-MATERIAL-0123456789!!"))
	cases := []struct {
		name        string
		env         map[string]string
		wantErr     bool
		wantWarn    bool
		errContains []string
		secrets     []string // must never appear in the error
	}{
		{name: "absent key is refused", env: map[string]string{},
			wantErr: true, errContains: []string{v2secrets.MasterKeyEnvVar, v2secrets.AllowUnwrappedEnvVar}},
		{name: "empty key is refused", env: map[string]string{v2secrets.MasterKeyEnvVar: ""},
			wantErr: true, errContains: []string{v2secrets.MasterKeyEnvVar}},
		{name: "absent key with opt-out false is refused",
			env:     map[string]string{v2secrets.AllowUnwrappedEnvVar: "false"},
			wantErr: true, errContains: []string{v2secrets.MasterKeyEnvVar}},
		{name: "absent key with opt-out is allowed with a warning",
			env:      map[string]string{v2secrets.AllowUnwrappedEnvVar: "true"},
			wantWarn: true},
		{name: "valid key is allowed", env: map[string]string{v2secrets.MasterKeyEnvVar: valid}},
		{name: "valid key with opt-out raises no warning",
			env: map[string]string{v2secrets.MasterKeyEnvVar: valid, v2secrets.AllowUnwrappedEnvVar: "true"}},
		{name: "malformed key is refused", env: map[string]string{v2secrets.MasterKeyEnvVar: "not a key"},
			wantErr: true, errContains: []string{v2secrets.MasterKeyEnvVar}, secrets: []string{"not a key"}},
		{name: "malformed key is refused even with the opt-out",
			env:     map[string]string{v2secrets.MasterKeyEnvVar: valid + " ", v2secrets.AllowUnwrappedEnvVar: "true"},
			wantErr: true, errContains: []string{v2secrets.MasterKeyEnvVar}, secrets: []string{valid}},
		{name: "wrong-length key is refused", env: map[string]string{v2secrets.MasterKeyEnvVar: shortKey},
			wantErr: true, errContains: []string{v2secrets.MasterKeyEnvVar}, secrets: []string{shortKey, "SHORT-KEY"}},
		{name: "unrecognised opt-out value is refused",
			env:     map[string]string{v2secrets.AllowUnwrappedEnvVar: "yes"},
			wantErr: true, errContains: []string{v2secrets.AllowUnwrappedEnvVar}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, err := requireVaultMasterKey(func(k string) string { return tc.env[k] })
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil {
				for _, want := range tc.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %s", err, want)
					}
				}
				for _, secret := range tc.secrets {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error leaks key material %q: %v", secret, err)
					}
				}
			}
			if (warn != "") != tc.wantWarn {
				t.Fatalf("warning = %q, want warning: %v", warn, tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(warn, v2secrets.MasterKeyEnvVar) {
				t.Errorf("warning %q does not name %s", warn, v2secrets.MasterKeyEnvVar)
			}
		})
	}
}
