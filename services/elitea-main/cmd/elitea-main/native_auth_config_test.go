package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

func nativeEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestNativeAuthFromEnvRefusesBadValuesAndAMissingOrigin(t *testing.T) {
	dir := t.TempDir()
	clients := filepath.Join(dir, "clients.json")
	if err := os.WriteFile(clients, []byte(`[{"client_id":"dev.elitea.boot","display_name":"Boot","redirect_uris":["dev.elitea.boot:/cb"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := &pgxpool.Pool{}

	// A registered client without DEPLOYMENT_URL refuses boot (decision 9).
	_, err := nativeAuthFromEnv(context.Background(),
		nativeEnv(map[string]string{nativeauth.NativeClientsPathEnv: clients}), pool, "", nil)
	if !errors.Is(err, errNativeNeedsPublicOrigin) {
		t.Fatalf("file client without origin = %v, want errNativeNeedsPublicOrigin", err)
	}
	// With the origin it composes all three parts.
	composed, err := nativeAuthFromEnv(context.Background(),
		nativeEnv(map[string]string{nativeauth.NativeClientsPathEnv: clients}), pool, "https://elitea.example", nil)
	if err != nil || composed.registry == nil || composed.store == nil || composed.apiTokens() == nil || composed.graphTokens() == nil {
		t.Fatalf("composed = %+v, %v", composed, err)
	}
	// A TTL out of range, or a file that does not parse, refuses boot and
	// names the variable.
	_, err = nativeAuthFromEnv(context.Background(),
		nativeEnv(map[string]string{nativeauth.AccessTokenTTLEnv: "2h"}), pool, "https://elitea.example", nil)
	if err == nil || !strings.Contains(err.Error(), nativeauth.AccessTokenTTLEnv) {
		t.Fatalf("bad TTL = %v", err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	_ = os.WriteFile(broken, []byte("- client_id: X\n"), 0o600)
	if _, err := nativeAuthFromEnv(context.Background(),
		nativeEnv(map[string]string{nativeauth.NativeClientsPathEnv: broken}), pool, "https://elitea.example", nil); err == nil {
		t.Fatal("an invalid clients file must refuse boot")
	}
	// No pool: nothing is composed, and the interfaces stay NIL interfaces.
	none, err := nativeAuthFromEnv(context.Background(), nativeEnv(nil), nil, "", nil)
	if err != nil || none.apiTokens() != nil || none.graphTokens() != nil || none.registry != nil {
		t.Fatalf("no pool = %+v, %v", none, err)
	}
}

type stubTokens struct{ name string }

func (s stubTokens) ValidateToken(context.Context, string) (auth.User, error) {
	return auth.User{ID: s.name}, nil
}

func TestWithNativeTokensWrapsOnlyAComposedPlane(t *testing.T) {
	native := stubTokens{name: "native"}
	if got := withNativeTokens(apimw.AuthConfig{}, native); got.Validator != nil {
		t.Fatal("a deployment with no credential plane must not start accepting native tokens")
	}
	composed := apimw.AuthConfig{Validator: stubTokens{name: "pat"}, PrincipalValidator: stubPrincipals{}, SessionSecret: "s"}
	wrapped := withNativeTokens(composed, native)
	for token, want := range map[string]string{"elnat_x": "native", "eyJ.x.y": "pat"} {
		user, err := wrapped.Validator.ValidateToken(context.Background(), token)
		if err != nil || user.ID != want {
			t.Fatalf("%s -> %+v, %v; want %s", token, user, err, want)
		}
	}
	if unchanged := withNativeTokens(composed, nil); unchanged.Validator != composed.Validator {
		t.Fatal("a nil native validator must leave the composition unchanged")
	}
}

type stubPrincipals struct{}

func (stubPrincipals) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}
