package authcomposition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
)

// DEFECT: the /api/v2/auth/token route signed a new personal access token with
// APPLICATION_SECRET_KEY, while this graph reads a personal access token back
// with the bytes of credentials.pat_signing_key_file. The two values are
// unrelated, so every token a form deployment issued failed the signature
// check on first use and answered 401 "token validation failed".
//
// The graph now signs the token itself, with the key its own validator uses.
// The key must survive materialize's destroy step, which wipes the snapshot
// when newFormGraph returns.
func TestFormGraphSignsAPATWithTheKeyItValidatesWith(t *testing.T) {
	config := writeMaterialFixture(t)
	pool := newUnconnectedPool(t)
	var patKey []byte

	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{
			PostgreSQL:           pool,
			MainRoutePublicRules: []forwardapp.PublicRule{},
		},
		func(material *materializedFiles) {
			// Copy: the snapshot is wiped before newFormGraph returns.
			patKey = append([]byte(nil), material.patSigningKey...)
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(patKey) == 0 {
		t.Fatal("the fixture produced no PAT signing key")
	}

	uuid := "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	expires := time.Now().UTC().Add(time.Hour)
	token, err := graph.SignPAT(&uuid, &expires)
	if err != nil {
		t.Fatalf("SignPAT: %v", err)
	}

	parsed, err := jwt.Parse(token,
		func(*jwt.Token) (any, error) { return patKey, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS512.Alg()}),
	)
	if err != nil {
		t.Fatalf("the signed token does not verify with credentials.pat_signing_key_file: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || claims["uuid"] != uuid {
		t.Fatalf("claims = %+v, want uuid %q", parsed.Claims, uuid)
	}

	// A token signed with any other secret must NOT verify. That is the whole
	// failure: APPLICATION_SECRET_KEY was the other secret.
	if _, err := jwt.Parse(token,
		func(*jwt.Token) (any, error) { return []byte("changeme-standalone"), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS512.Alg()}),
	); err == nil {
		t.Fatal("the signed token verifies with an unrelated secret")
	}
}

// A graph with no key must refuse to sign. It must never return an unsigned
// or empty bearer.
func TestFormGraphSignPATRefusesWithoutAKey(t *testing.T) {
	if _, err := (*FormGraph)(nil).SignPAT(nil, nil); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("nil graph error = %v, want ErrInvalidGraph", err)
	}
	if _, err := (&FormGraph{}).SignPAT(nil, nil); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("keyless graph error = %v, want ErrInvalidGraph", err)
	}
}
