package spi_test

import (
	"context"
	"net/http"
	"testing"

	subappv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi/spitest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// With no secret there is nothing to verify against: no header set
// verifies, signed or not (#2959).
func TestVerifySignatureRefusesEveryIdentityWithoutASecret(t *testing.T) {
	signed := http.Header{}
	spi.SignHeaders(signed, spi.Identity{ProjectID: "1", UserID: "7"}, []byte(sharedSecret))
	for name, h := range map[string]http.Header{
		"no headers": {},
		"unsigned":   {spi.HeaderProjectID: {"1"}},
		"signed":     signed,
	} {
		if spi.VerifySignature(h, nil) || spi.VerifySignature(h, []byte{}) {
			t.Errorf("%s: verified with no secret", name)
		}
	}
}

// A host built with no identity secret and without the development flag
// refuses every identity-bearing call, even one whose metadata claims a
// project (#2959). cmd/elitea-subapp-host does not start that way; this is
// the server's own defence for a host composed without that check.
func TestAHostWithoutASecretRefusesEveryCall(t *testing.T) {
	settings, err := spi.SettingsFromEnv(echo.EnvPrefix, env(map[string]string{spi.InsecureIdentityEnvVar: "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if settings.InsecureIdentity {
		t.Fatal("the development flag reads as set")
	}
	server, err := spi.NewServer(settings, echo.App(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	l := spitest.Start(t, server)

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-elitea-project-id", "999"))
	if _, err := l.Provider().Describe(ctx, &subappv1.DescribeRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Describe on a host without a secret: %v, want Unauthenticated", err)
	}
	if _, err := l.Provider().Health(ctx, &subappv1.HealthRequest{}); err != nil {
		t.Fatalf("Health stays open (no identity needed): %v", err)
	}
}

func TestTheInsecureIdentityFlagMustBeABoolean(t *testing.T) {
	if _, err := spi.SettingsFromEnv(echo.EnvPrefix, env(map[string]string{spi.InsecureIdentityEnvVar: "maybe"})); err == nil {
		t.Fatal("a non-boolean development flag was accepted")
	}
}
