package main

// The start-up gate for SECRETS_MASTER_KEY, the gateway half of elitea-main's
// (services/elitea-main/cmd/elitea-main/master_key_startup_test.go).

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
)

// testMasterKey is a well-formed Fernet key that protects nothing. Tests that
// start the real process pass it so they reach the code they are about.
var testMasterKey = base64.URLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestRequireVaultMasterKey(t *testing.T) {
	t.Parallel()

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
			wantErr: true, errContains: []string{account.MasterKeyEnvVar, account.AllowUnwrappedEnvVar}},
		{name: "empty key is refused", env: map[string]string{account.MasterKeyEnvVar: ""},
			wantErr: true, errContains: []string{account.MasterKeyEnvVar}},
		{name: "absent key with opt-out false is refused",
			env:     map[string]string{account.AllowUnwrappedEnvVar: "false"},
			wantErr: true, errContains: []string{account.MasterKeyEnvVar}},
		{name: "absent key with opt-out is allowed with a warning",
			env:      map[string]string{account.AllowUnwrappedEnvVar: "true"},
			wantWarn: true},
		{name: "valid key is allowed", env: map[string]string{account.MasterKeyEnvVar: testMasterKey}},
		{name: "valid key with opt-out raises no warning",
			env: map[string]string{account.MasterKeyEnvVar: testMasterKey, account.AllowUnwrappedEnvVar: "true"}},
		{name: "malformed key is refused", env: map[string]string{account.MasterKeyEnvVar: "not a key"},
			wantErr: true, errContains: []string{account.MasterKeyEnvVar}, secrets: []string{"not a key"}},
		{name: "malformed key is refused even with the opt-out",
			env:     map[string]string{account.MasterKeyEnvVar: testMasterKey + " ", account.AllowUnwrappedEnvVar: "true"},
			wantErr: true, errContains: []string{account.MasterKeyEnvVar}, secrets: []string{testMasterKey}},
		{name: "wrong-length key is refused", env: map[string]string{account.MasterKeyEnvVar: shortKey},
			wantErr: true, errContains: []string{account.MasterKeyEnvVar}, secrets: []string{shortKey, "SHORT-KEY"}},
		{name: "unrecognised opt-out value is refused",
			env:     map[string]string{account.AllowUnwrappedEnvVar: "yes"},
			wantErr: true, errContains: []string{account.AllowUnwrappedEnvVar}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
			if tc.wantWarn && !strings.Contains(warn, account.MasterKeyEnvVar) {
				t.Errorf("warning %q does not name %s", warn, account.MasterKeyEnvVar)
			}
		})
	}
}

// masterKeySubprocessEnv is the marker that turns this test binary into the
// gateway itself.
const masterKeySubprocessEnv = "GATEWAY_MASTER_KEY_GATE_SUBPROCESS"

// TestGatewayRefusesToStartWithoutAMasterKey is the acceptance criterion: the
// PROCESS exits non-zero, before the listener and with no database pool, when
// SECRETS_MASTER_KEY is absent and the development opt-out is not set. The
// refusal is an os.Exit in the composition root, so only a process can prove it.
func TestGatewayRefusesToStartWithoutAMasterKey(t *testing.T) {
	if os.Getenv(masterKeySubprocessEnv) == "1" {
		main()
		return
	}

	command := func(ctx context.Context, masterKey, optOut string) (*exec.Cmd, *bytes.Buffer) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGatewayRefusesToStartWithoutAMasterKey$")
		cmd.Env = append(os.Environ(),
			masterKeySubprocessEnv+"=1",
			account.MasterKeyEnvVar+"="+masterKey,
			account.AllowUnwrappedEnvVar+"="+optOut,
			// Not a URL, so pgxpool.New fails and the pool is nil: the gate
			// must fire without a database, and no other check needs one.
			"DATABASE_URL=not-a-database-url",
			"GATEWAY_NATS_URL=",
			"LLM_BUDGET_REQUIRE_ENFORCEMENT="+config.RequireEnforcementOff,
			"GATEWAY_IDENTITY_SECRET=master-key-gate-test",
			"GATEWAY_HTTP_ADDR=127.0.0.1:0",
		)
		out := &bytes.Buffer{}
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd, out
	}

	t.Run("absent key refuses", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd, out := command(ctx, "", "")
		err := cmd.Run()
		if ctx.Err() != nil {
			t.Fatalf("the gateway kept running without a master key; output:\n%s", out.String())
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
			t.Fatalf("the gateway exited with %v without a master key; it must refuse; output:\n%s", err, out.String())
		}
		for _, want := range []string{"FATAL: refusing to start", account.MasterKeyEnvVar, account.AllowUnwrappedEnvVar} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("the refusal does not contain %q, so the process stopped for another reason; output:\n%s",
					want, out.String())
			}
		}
	})

	// The negative controls. Without them the subtest above proves only that
	// this environment cannot start a gateway. The SAME environment with a key,
	// or with the development opt-out, must keep running.
	for _, control := range []struct{ name, key, optOut string }{
		{"a valid key keeps serving in the same environment", testMasterKey, ""},
		{"the development opt-out keeps serving in the same environment", "", "true"},
	} {
		t.Run(control.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd, out := command(ctx, control.key, control.optOut)
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting the gateway subprocess: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				t.Fatalf("the gateway exited (%v), so the refusal above is not attributable to the master key "+
					"gate; output:\n%s", err, out.String())
			case <-time.After(3 * time.Second):
			}
			cancel()
			<-done
			if control.optOut == "true" && !strings.Contains(out.String(), "UNWRAPPED") {
				t.Errorf("the development opt-out started silently; it must log the unwrapped posture; output:\n%s",
					out.String())
			}
		})
	}
}
