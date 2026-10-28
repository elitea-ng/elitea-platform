package agentidentity

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// fakeDefinitions answers the computed digest of version 4 only, and
// counts the reads.
type fakeDefinitions struct {
	digest [32]byte
	err    error
	reads  int
}

func (f *fakeDefinitions) DefinitionDigest(_ context.Context, _, _, version int64) ([32]byte, bool, error) {
	f.reads++
	if f.err != nil {
		return [32]byte{}, false, f.err
	}
	return f.digest, version == 4, nil
}

// listingService lists two bindings of version 4 and one of version 5.
type listingService struct{ fakeService }

func (s *listingService) ListBindings(context.Context, agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error) {
	out := []agentidentity.AgentIdentityBinding{}
	for _, version := range []int64{4, 4, 5} {
		b := s.binding()
		b.TenantProjectID, b.ApplicationID, b.ApplicationVersionID = 7, 3, version
		out = append(out, b)
	}
	return out, nil
}

func (s *listingService) ProposeBinding(ctx context.Context, p agentidentity.Proposal, by string) (agentidentity.AgentIdentityBinding, error) {
	b, err := s.fakeService.ProposeBinding(ctx, p, by)
	b.TenantProjectID, b.ApplicationID, b.ApplicationVersionID = p.TenantProjectID, p.ApplicationID, p.ApplicationVersionID
	return b, err
}

func digestRouter(t *testing.T, service Service, definitions DefinitionDigests) http.Handler {
	t.Helper()
	h, err := New(service)
	if err != nil {
		t.Fatal(err)
	}
	h.WithDefinitionDigests(definitions)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "token-7", UserID: "42"})))
		})
	})
	r.Get("/agent_identities", h.ListBindings)
	r.Post("/agent_identities", h.CreateBinding)
	r.Post("/agent_identities/{id}:approve", h.ApproveBinding)
	r.Post("/agent_identities/{id}:repin", h.RepinBinding)
	return r
}

// #1462: :repin pins the computed digest of the binding's own version and
// returns it as a proposal of the caller; refusals are 409, a missing
// reader 503.
func TestRepinPinsTheComputedDigest(t *testing.T) {
	definitions := &fakeDefinitions{digest: [32]byte{0xcd}}
	want := hex.EncodeToString(definitions.digest[:])
	service := &fakeService{}
	h := digestRouter(t, service, definitions)

	rec := serve(h, http.MethodPost, "/agent_identities/"+bindingID+":repin", "")
	var view bindingView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("repin = %d %s", rec.Code, rec.Body)
	}
	if service.lastID != bindingID || service.lastActor != "42" || view.DefinitionDigest != want ||
		view.ComputedDefinitionDigest == nil || *view.ComputedDefinitionDigest != want ||
		view.State != string(agentidentity.BindingProposed) || view.ProposedBy != "42" || view.ApprovedBy != nil {
		t.Fatalf("repinned %+v (service %+v)", view, service)
	}

	for name, tc := range map[string]struct {
		service     *fakeService
		definitions DefinitionDigests
		want        int
	}{
		"already pinned":     {&fakeService{err: agentidentity.ErrDefinitionDigestCurrent}, definitions, http.StatusConflict},
		"reader down":        {&fakeService{}, &fakeDefinitions{err: errors.New("down")}, http.StatusInternalServerError},
		"revoked binding":    {&fakeService{err: agentidentity.ErrInvalidTransition}, definitions, http.StatusConflict},
		"no digest reader":   {&fakeService{}, nil, http.StatusServiceUnavailable},
		"self or mode guard": {&fakeService{err: agentidentity.ErrIdentityModeNotAllowed}, definitions, http.StatusForbidden},
	} {
		var reader DefinitionDigests
		if tc.definitions != nil {
			reader = tc.definitions
		}
		rec := serve(digestRouter(t, tc.service, reader), http.MethodPost, "/agent_identities/"+bindingID+":repin", "")
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// A version the tenant does not hold cannot be re-pinned: 409.
func TestRepinOfAnUnreadableVersionConflicts(t *testing.T) {
	service := &unreadableVersion{}
	rec := serve(digestRouter(t, service, &fakeDefinitions{digest: [32]byte{1}}), http.MethodPost, "/agent_identities/"+bindingID+":repin", "")
	if rec.Code != http.StatusConflict || !errors.Is(service.err, agentidentity.ErrDefinitionUnreadable) {
		t.Fatalf("repin = %d %s (%v)", rec.Code, rec.Body, service.err)
	}
}

// unreadableVersion locks a binding of version 5, which fakeDefinitions
// does not hold.
type unreadableVersion struct {
	fakeService
	err error
}

func (s *unreadableVersion) RepinBinding(ctx context.Context, _, _ string,
	digestOf func(context.Context, agentidentity.AgentIdentityBinding) ([32]byte, error)) (agentidentity.AgentIdentityBinding, error) {
	current := s.binding()
	current.ApplicationVersionID = 5
	_, s.err = digestOf(ctx, current)
	return agentidentity.AgentIdentityBinding{}, s.err
}

// #1462: every binding view carries the computed digest of its version, so
// an operator can re-pin; a version that cannot be read shows null; each
// version is read once per response.
func TestBindingViewsCarryTheComputedDefinitionDigest(t *testing.T) {
	definitions := &fakeDefinitions{digest: [32]byte{0xab}}
	want := hex.EncodeToString(definitions.digest[:])
	h := digestRouter(t, &listingService{}, definitions)

	rec := serve(h, http.MethodGet, "/agent_identities", "")
	var list struct{ Rows []bindingView }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Rows) != 3 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	for i, row := range list.Rows[:2] {
		if row.ComputedDefinitionDigest == nil || *row.ComputedDefinitionDigest != want {
			t.Fatalf("row %d computed = %v", i, row.ComputedDefinitionDigest)
		}
	}
	if list.Rows[2].ComputedDefinitionDigest != nil {
		t.Fatalf("an unreadable version shows %v", *list.Rows[2].ComputedDefinitionDigest)
	}
	if definitions.reads != 2 {
		t.Fatalf("reads = %d, want one per version", definitions.reads)
	}

	body := `{"tenant_project_id":7,"application_id":3,"application_version_id":4,"definition_digest":"` +
		strings.Repeat("0", 64) + `","mode":"dedicated_agent"}`
	rec = serve(h, http.MethodPost, "/agent_identities", body)
	var created bindingView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if created.DefinitionDigest != strings.Repeat("0", 64) || created.ComputedDefinitionDigest == nil {
		t.Fatalf("created = %+v", created)
	}
}

// A read failure is not an error of the admin call: the field is null.
func TestComputedDigestFailureLeavesTheFieldNull(t *testing.T) {
	h := digestRouter(t, &listingService{}, &fakeDefinitions{err: errors.New("down")})
	rec := serve(h, http.MethodGet, "/agent_identities", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"computed_definition_digest":null`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	// Without the reader the field is present and null.
	plain := digestRouter(t, &listingService{}, nil)
	if rec := serve(plain, http.MethodPost, "/agent_identities/"+bindingID+":approve", "{}"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"computed_definition_digest":null`) {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
}
