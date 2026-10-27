package credentialprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	refID     = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"
	targetID  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22"
	grantID   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f33"
	tenant    = int64(7)
	secretVal = "s3cr3t-provider-token-value"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type fakeReferences struct {
	ref credentialgrant.CredentialReference
	err error
}

func (f fakeReferences) CredentialReference(_ context.Context, tenantProjectID int64, id string) (credentialgrant.CredentialReference, error) {
	if f.err != nil {
		return credentialgrant.CredentialReference{}, f.err
	}
	if tenantProjectID != f.ref.TenantProjectID || id != f.ref.ID {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	return f.ref, nil
}

// fakeGrants returns its grants as stored; the handler must filter them.
type fakeGrants struct {
	grants []credentialgrant.CredentialUseGrant
}

func (f fakeGrants) ActiveGrantsWithCapability(context.Context, int64, string, credentialgrant.Capability) ([]credentialgrant.CredentialUseGrant, error) {
	return f.grants, nil
}

type fakeAudit struct {
	events []securityaudit.SecurityEvent
	err    error
}

func (f *fakeAudit) RecordProbe(_ context.Context, e securityaudit.SecurityEvent) error {
	f.events = append(f.events, e)
	return f.err
}

// materialProber holds material, as a real prober would during the call,
// and leaks it in its error when told to.
type materialProber struct {
	result  credentialadapters.ProbeResult
	fail    bool
	calls   int
	request credentialadapters.ProbeRequest
}

func (p *materialProber) Probe(_ context.Context, req credentialadapters.ProbeRequest) (credentialadapters.ProbeResult, error) {
	p.calls++
	p.request = req
	if p.fail {
		return credentialadapters.ProbeResult{}, errors.New("provider said: bad token " + secretVal)
	}
	return p.result, nil
}

func reference() credentialgrant.CredentialReference {
	return credentialgrant.CredentialReference{
		ID: refID, TenantProjectID: tenant, Class: credentialgrant.ClassStaticSecret, Backend: "pgvault",
		Locator: "governed/jira/" + secretVal, ProviderType: "jira", TargetBindingID: targetID,
		Generation: 3, State: credentialgrant.ReferenceActive,
	}
}

func grantWith(caps ...credentialgrant.Capability) credentialgrant.CredentialUseGrant {
	return credentialgrant.CredentialUseGrant{
		ID: grantID, TenantProjectID: tenant, CredentialReferenceID: refID, TargetBindingID: targetID,
		Capabilities: caps, State: credentialgrant.GrantActive,
	}
}

type setup struct {
	ref    credentialgrant.CredentialReference
	grants []credentialgrant.CredentialUseGrant
	prober credentialadapters.Prober
	audit  *fakeAudit
}

func serve(t *testing.T, s setup, user *auth.User, path string) *httptest.ResponseRecorder {
	t.Helper()
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if s.audit == nil {
		s.audit = &fakeAudit{}
	}
	h, err := New(Config{
		References: fakeReferences{ref: s.ref}, Grants: fakeGrants{grants: s.grants}, Audit: s.audit,
		Descriptors: registry, Prober: s.prober, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Post("/api/v2/projects/{projectID}/credential_references/{id}:probe", h.Probe)
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if user != nil {
		req = req.WithContext(auth.ContextWithUser(req.Context(), *user))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func probePath() string { return "/api/v2/projects/7/credential_references/" + refID + ":probe" }

var caller = &auth.User{ID: "42", UserID: "42"}

func TestProbeNeedsTheProbeCapabilityNotUse(t *testing.T) {
	audit := &fakeAudit{}
	prober := &materialProber{result: credentialadapters.ProbeResult{Outcome: credentialadapters.ProbeSucceeded}}
	rec := serve(t, setup{ref: reference(), grants: []credentialgrant.CredentialUseGrant{grantWith(credentialgrant.CapUse)},
		prober: prober, audit: audit}, caller, probePath())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("use-only grant: status %d, want 403: %s", rec.Code, rec.Body)
	}
	if prober.calls != 0 {
		t.Fatal("the prober ran without a probe grant")
	}
	if len(audit.events) != 1 || audit.events[0].Effect != securityaudit.EffectDeny || audit.events[0].Type != securityaudit.TypeProbe {
		t.Fatalf("deny audit = %+v", audit.events)
	}
}

func TestProbeRefusesGrantsThatDoNotCoverTheReference(t *testing.T) {
	expired := grantWith(credentialgrant.CapProbe)
	past := now.Add(-time.Minute)
	expired.ExpiresAt = &past
	suspended := grantWith(credentialgrant.CapProbe)
	suspended.State = credentialgrant.GrantSuspended
	otherTarget := grantWith(credentialgrant.CapProbe)
	otherTarget.TargetBindingID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f99"
	otherTenant := grantWith(credentialgrant.CapProbe)
	otherTenant.TenantProjectID = 8
	for name, g := range map[string]credentialgrant.CredentialUseGrant{
		"expired": expired, "suspended": suspended, "other target": otherTarget, "other tenant": otherTenant,
	} {
		rec := serve(t, setup{ref: reference(), grants: []credentialgrant.CredentialUseGrant{g}}, caller, probePath())
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
}

// The response never holds material: not on success, not when the prober
// fails with the material in its error text.
func TestProbeResponseNeverContainsMaterial(t *testing.T) {
	for name, prober := range map[string]*materialProber{
		"succeeded":   {result: credentialadapters.ProbeResult{Outcome: credentialadapters.ProbeSucceeded, ProviderStatus: 200}},
		"rejected":    {result: credentialadapters.ProbeResult{Outcome: credentialadapters.ProbeRejected, ProviderStatus: 401}},
		"failed":      {fail: true},
		"bad outcome": {result: credentialadapters.ProbeResult{Outcome: credentialadapters.ProbeOutcome(secretVal)}},
	} {
		t.Run(name, func(t *testing.T) {
			audit := &fakeAudit{}
			rec := serve(t, setup{ref: reference(), grants: []credentialgrant.CredentialUseGrant{grantWith(credentialgrant.CapUse, credentialgrant.CapProbe)},
				prober: prober, audit: audit}, caller, probePath())
			if strings.Contains(rec.Body.String(), secretVal) || strings.Contains(rec.Body.String(), "governed/jira") {
				t.Fatalf("the response leaks material or the locator: %s", rec.Body)
			}
			if prober.calls != 1 || prober.request.Operation != "jira.project.read" || prober.request.Reference.ID != refID {
				t.Fatalf("prober request = %+v (calls %d)", prober.request, prober.calls)
			}
			for _, e := range audit.events {
				if strings.Contains(strings.Join(append(e.Reasons, e.ResourceID, e.GrantRef), " "), secretVal) {
					t.Fatalf("the audit event leaks material: %+v", e)
				}
			}
			if name == "failed" || name == "bad outcome" {
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status %d, want 502", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{"credential_reference_id": true, "provider_type": true, "operation": true,
				"outcome": true, "message": true, "provider_status": true, "checked_at": true}
			for key := range body {
				if !want[key] {
					t.Errorf("unexpected response field %q", key)
				}
			}
			if body["outcome"] != name || body["operation"] != "jira.project.read" {
				t.Errorf("body = %v", body)
			}
			if len(audit.events) != 1 || audit.events[0].Effect != securityaudit.EffectAllow ||
				audit.events[0].GrantRef != grantID || audit.events[0].Reasons[0] != "probe."+name {
				t.Errorf("allow audit = %+v", audit.events)
			}
		})
	}
}

// No prober configured: the fail-closed default answers "unavailable" and
// reads nothing.
func TestProbeWithoutAProberIsUnavailable(t *testing.T) {
	rec := serve(t, setup{ref: reference(), grants: []credentialgrant.CredentialUseGrant{grantWith(credentialgrant.CapProbe)}}, caller, probePath())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body probeView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Outcome != "unavailable" || body.Message != "probe not available for provider" || body.ProviderStatus != nil {
		t.Fatalf("body = %+v", body)
	}
}

func TestProbeRefusals(t *testing.T) {
	probeGrants := []credentialgrant.CredentialUseGrant{grantWith(credentialgrant.CapProbe)}
	retired := reference()
	retired.State = credentialgrant.ReferenceRetired
	unknownProvider := reference()
	unknownProvider.ProviderType = "nope"
	wrongClass := reference()
	wrongClass.ProviderType = "sharepoint" // oauth_delegated only
	cases := []struct {
		name string
		s    setup
		user *auth.User
		path string
		want int
	}{
		{"anonymous", setup{ref: reference(), grants: probeGrants}, nil, probePath(), http.StatusUnauthorized},
		{"no user id", setup{ref: reference(), grants: probeGrants}, &auth.User{ID: "x"}, probePath(), http.StatusForbidden},
		{"bad project", setup{ref: reference(), grants: probeGrants}, caller, "/api/v2/projects/07/credential_references/" + refID + ":probe", http.StatusBadRequest},
		{"other project", setup{ref: reference(), grants: probeGrants}, caller, "/api/v2/projects/8/credential_references/" + refID + ":probe", http.StatusNotFound},
		{"unknown reference", setup{ref: reference(), grants: probeGrants}, caller, "/api/v2/projects/7/credential_references/nope:probe", http.StatusNotFound},
		{"retired reference", setup{ref: retired, grants: probeGrants}, caller, probePath(), http.StatusConflict},
		{"no adapter", setup{ref: unknownProvider, grants: probeGrants}, caller, probePath(), http.StatusConflict},
		{"class not served", setup{ref: wrongClass, grants: probeGrants}, caller, probePath(), http.StatusConflict},
		{"audit down", setup{ref: reference(), grants: probeGrants, audit: &fakeAudit{err: errors.New("down")}}, caller, probePath(), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if rec := serve(t, c.s, c.user, c.path); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d: %s", c.name, rec.Code, c.want, rec.Body)
		}
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("an empty config was accepted")
	}
}
