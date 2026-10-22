package material_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

type revokingMinter struct {
	minted, revoked []string
}

func (m *revokingMinter) Mint(_ context.Context, _, _ int64, _ string, _ time.Duration) (material.Grant, error) {
	uuid := "u-" + string(rune('a'+len(m.minted)))
	m.minted = append(m.minted, uuid)
	return material.Grant{Bearer: "bearer", UUID: uuid, Expires: time.Now().Add(time.Hour)}, nil
}

func (m *revokingMinter) Revoke(_ context.Context, _ int64, uuid string) error {
	m.revoked = append(m.revoked, uuid)
	return nil
}

type invocationGrants struct {
	recorded []repos.CallbackInvocationGrant
	err      error
}

func (g *invocationGrants) RecordInvocation(_ context.Context, grant repos.CallbackInvocationGrant) error {
	if g.err != nil {
		return g.err
	}
	g.recorded = append(g.recorded, grant)
	return nil
}

// Every mint records the invocation grant: the provider, the tool the route
// named, the owner and the bound project.
func TestAGrantingMinterRecordsEveryMint(t *testing.T) {
	inner := &revokingMinter{}
	grants := &invocationGrants{}
	minter, err := material.GrantingMinter(inner, grants, "deepwiki")
	if err != nil {
		t.Fatal(err)
	}
	ctx := material.WithInvocationTool(context.Background(), "generate_wiki")
	grant, err := minter.Mint(ctx, 11, 42, "deepwiki callback (project 42)", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := minter.Mint(context.Background(), 11, 42, "n", time.Hour); err != nil {
		t.Fatal(err)
	}
	want := []repos.CallbackInvocationGrant{
		{TokenUUID: grant.UUID, OwnerID: 11, ProjectID: 42, Provider: "deepwiki", Tool: "generate_wiki"},
		{TokenUUID: inner.minted[1], OwnerID: 11, ProjectID: 42, Provider: "deepwiki", Tool: material.UnnamedInvocationTool},
	}
	if len(grants.recorded) != 2 || grants.recorded[0] != want[0] || grants.recorded[1] != want[1] {
		t.Fatalf("recorded %+v, want %+v", grants.recorded, want)
	}
	if len(inner.revoked) != 0 {
		t.Fatalf("revoked %v", inner.revoked)
	}
}

// A grant that cannot be recorded revokes the bearer it was for and fails the
// mint: an ungranted bearer would reach the engine and be refused by
// elitea-vector at the end of the run.
func TestAnUnrecordedGrantRevokesTheBearerAndFailsTheMint(t *testing.T) {
	inner := &revokingMinter{}
	minter, err := material.GrantingMinter(inner, &invocationGrants{err: repos.ErrCallbackGrantNotRecorded}, "inventory")
	if err != nil {
		t.Fatal(err)
	}
	grant, err := minter.Mint(context.Background(), 11, 42, "n", time.Hour)
	if !errors.Is(err, material.ErrSourceUnavailable) || !errors.Is(err, repos.ErrCallbackGrantNotRecorded) {
		t.Fatalf("err = %v", err)
	}
	if grant.Bearer != "" || len(inner.revoked) != 1 || inner.revoked[0] != inner.minted[0] {
		t.Fatalf("grant %+v, revoked %v of %v", grant, inner.revoked, inner.minted)
	}
}

func TestAGrantingMinterRefusesHalfWiring(t *testing.T) {
	if minter, err := material.GrantingMinter(nil, &invocationGrants{}, "deepwiki"); minter != nil || err != nil {
		t.Fatalf("nil minter: %v %v", minter, err)
	}
	if _, err := material.GrantingMinter(&revokingMinter{}, nil, "deepwiki"); err == nil {
		t.Fatal("a nil recorder was accepted")
	}
	if _, err := material.GrantingMinter(&revokingMinter{}, &invocationGrants{}, ""); err == nil {
		t.Fatal("an empty provider was accepted")
	}
}

// Invocation.Serve names the route's tool for the grant the rewrite's mint
// records.
func TestServeNamesTheInvokedToolForTheGrant(t *testing.T) {
	inner := &revokingMinter{}
	grants := &invocationGrants{}
	minter, err := material.GrantingMinter(inner, grants, "deepwiki")
	if err != nil {
		t.Fatal(err)
	}
	invocation := material.Invocation{
		Provider: "DeepWiki",
		Rewrite:  material.CallbackOnly("deepwiki", minter, material.PlatformRouteBase, time.Hour),
		Forward: func(w http.ResponseWriter, _ *http.Request, _, _, _ string) {
			w.WriteHeader(http.StatusAccepted)
		},
		Path:   func(*http.Request) string { return "/invoke" },
		Minter: minter,
		Status: func(error) (int, string) { return http.StatusServiceUnavailable, "no" },
	}
	router := chi.NewRouter()
	router.Post("/{project_id}/{toolkit_name}/{tool_name}", invocation.Serve)
	request := httptest.NewRequest(http.MethodPost, "/42/Wikis/generate_wiki",
		strings.NewReader(`{"parameters":{},"configuration":{}}`))
	request = request.WithContext(auth.ContextWithAuthenticatedUser(request.Context(),
		auth.User{ID: "11", UserID: "11"}, auth.AuthenticationSourceSession))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	body, _ := io.ReadAll(recorder.Body)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", recorder.Code, body)
	}
	if len(grants.recorded) != 1 || grants.recorded[0].Tool != "generate_wiki" ||
		grants.recorded[0].ProjectID != 42 || grants.recorded[0].OwnerID != 11 {
		t.Fatalf("recorded %+v", grants.recorded)
	}
}
