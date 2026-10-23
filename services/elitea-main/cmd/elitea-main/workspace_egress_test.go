package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egresstoken"
)

func egressKeyFile(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, err := runtoken.MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signing.pem")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pub
}

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadWorkspaceEgressLoadsTheKeyAndLogsItsID(t *testing.T) {
	path, pub := egressKeyFile(t)
	var logs bytes.Buffer
	egress, err := loadWorkspaceEgress(envOf(map[string]string{
		envEgressProxyURL:             "http://elitea-egress-proxy.platform.svc:3128",
		egresstoken.EnvSigningKeyFile: path,
		envEgressDefaultTurnWall:      "45m",
	}), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if egress.Minter == nil || egress.DefaultTurnWall != 45*time.Minute {
		t.Fatalf("egress: %+v", egress)
	}
	if !strings.Contains(logs.String(), runtoken.KeyID(pub)) {
		t.Fatalf("the key id is not logged: %s", logs.String())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(logs.String(), string(data[30:60])) {
		t.Fatal("key material was logged")
	}
}

func TestLoadWorkspaceEgressUnconfiguredIsNoProxy(t *testing.T) {
	egress, err := loadWorkspaceEgress(envOf(nil), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil || egress.Minter != nil || egress.ProxyURL != "" {
		t.Fatalf("%+v %v", egress, err)
	}
}

func TestLoadWorkspaceEgressHalfConfiguredWarns(t *testing.T) {
	var logs bytes.Buffer
	egress, err := loadWorkspaceEgress(envOf(map[string]string{envEgressProxyURL: "http://proxy:3128"}),
		slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || egress.Minter != nil {
		t.Fatalf("%+v %v", egress, err)
	}
	if !strings.Contains(logs.String(), "refused") {
		t.Fatalf("no warning: %s", logs.String())
	}
}

func TestLoadWorkspaceEgressRefusesABadConfiguration(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"credentials in the url": {envEgressProxyURL: "http://run:x@proxy:3128"},
		"https url":              {envEgressProxyURL: "https://proxy:3128"},
		"missing key file":       {egresstoken.EnvSigningKeyFile: filepath.Join(t.TempDir(), "absent.pem")},
		"not a key":              {egresstoken.EnvSigningKeyFile: bad},
		"bad wall":               {envEgressDefaultTurnWall: "forever"},
	} {
		if _, err := loadWorkspaceEgress(envOf(env), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
