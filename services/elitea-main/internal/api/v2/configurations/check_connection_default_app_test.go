package configurations

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

// The Default app of ADR-0057 decision 5 in the credential check: the credential names
// `oauth_app_id`, the check resolves the client from the admin registry, and an app that
// cannot be used is refused with its stable code and never replaced by another client.

const testAppID = "11111111-1111-4111-8111-111111111111"

type fakeDescriber struct {
	resolved microsoftapps.Resolved
	err      error
	asked    []microsoftapps.Request
}

func (f *fakeDescriber) Describe(_ context.Context, req microsoftapps.Request) (microsoftapps.Resolved, error) {
	f.asked = append(f.asked, req)
	return f.resolved, f.err
}

func appHandler(t *testing.T, describer *fakeDescriber, served map[string]string) (*Handler, *[]string) {
	t.Helper()
	handler, requested := newMicrosoftHandler(served, nil)
	WithMicrosoftApps(describer)(handler)
	return handler, requested
}

func resolvedApp(authority string, refresh bool, ceiling []string) microsoftapps.Resolved {
	return microsoftapps.Resolved{AppID: testAppID, ClientID: "7f8d0c1e-0000-4000-8000-000000000001", Authority: authority, AutoRefreshToken: refresh, Ceiling: ceiling}
}

func TestADefaultAppSignsInWithTheAppsAuthorityAndItsCeiling(t *testing.T) {
	describer := &fakeDescriber{resolved: resolvedApp(entraTenant, true, []string{"Mail.Read"})}
	handler, _ := appHandler(t, describer, goodEntraDiscovery())

	// The credential also carries a client of its own and a hostile authority: a Default app
	// credential never reads them, so neither is used.
	recorder := postDelegatedCheck(t, handler, "outlook", encode(t, map[string]any{
		"oauth_app_id": testAppID, "scopes": []string{"Mail.Send", "Mail.Read"},
		"client_id": "someone-elses", "client_secret": plantedSecret, "oauth_discovery_endpoint": "https://evil.example.test/tenant",
	}))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeCheck(t, recorder.Body.Bytes())
	if body.AuthMetadata.ServerURL != entraTenant {
		t.Errorf("authority = %q, want the app's", body.AuthMetadata.ServerURL)
	}
	// Mail.Send is above the app's ceiling and is not asked for; offline_access follows the APP.
	if got := body.AuthMetadata.ResourceMetadata.ScopesSupport; !reflect.DeepEqual(got, []string{"offline_access", "Mail.Read"}) {
		t.Errorf("scopes = %v", got)
	}
	if strings.Contains(recorder.Body.String(), plantedSecret) || strings.Contains(recorder.Body.String(), "someone-elses") {
		t.Errorf("the credential's own client reached the answer: %s", recorder.Body.String())
	}
	want := microsoftapps.Request{AppID: testAppID, CredentialType: "outlook", ProjectID: 7}
	if len(describer.asked) != 1 || describer.asked[0] != want {
		t.Errorf("the registry was asked %+v, want %+v", describer.asked, want)
	}
}

func TestTheAppsRefreshSettingDecidesOfflineAccess(t *testing.T) {
	describer := &fakeDescriber{resolved: resolvedApp(entraTenant, false, []string{"Mail.Read"})}
	handler, _ := appHandler(t, describer, goodEntraDiscovery())
	// The credential's own auto_refresh_token does not apply under a Default app.
	recorder := postDelegatedCheck(t, handler, "outlook", encode(t, map[string]any{"oauth_app_id": testAppID, "auto_refresh_token": true}))
	body := decodeCheck(t, recorder.Body.Bytes())
	if got := body.AuthMetadata.ResourceMetadata.ScopesSupport; !reflect.DeepEqual(got, []string{"Mail.Read"}) {
		t.Errorf("scopes = %v, want no offline_access", got)
	}
}

func TestADefaultAppDataverseUsesTheCredentialsTenantAndItsOrgScope(t *testing.T) {
	const tenant = "https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555"
	describer := &fakeDescriber{resolved: resolvedApp(tenant, true, nil)}
	handler, _ := appHandler(t, describer, entraDiscovery(tenant, tenant+"/oauth2/v2.0/authorize", tenant+"/oauth2/v2.0/token"))
	recorder := postDelegatedCheck(t, handler, "dataverse", encode(t, map[string]any{
		"oauth_app_id": testAppID, "url": dataverseOrg, "tenant_id": "11111111-2222-4333-8444-555555555555",
	}))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeCheck(t, recorder.Body.Bytes())
	if got := body.AuthMetadata.ResourceMetadata.ScopesSupport; !reflect.DeepEqual(got, []string{"offline_access", dataverseOrg + "/user_impersonation"}) {
		t.Errorf("scopes = %v", got)
	}
	if len(describer.asked) != 1 || describer.asked[0].TenantID != "11111111-2222-4333-8444-555555555555" || describer.asked[0].CredentialType != "dataverse" {
		t.Errorf("the registry was asked %+v", describer.asked)
	}
}

// A refusal carries the registry's stable code, fetches nothing, and does not fall back.
func TestAnAppThatCannotBeUsedIsRefusedWithItsCodeAndNothingIsFetched(t *testing.T) {
	for _, code := range []string{
		microsoftapps.CodeAppNotFound, microsoftapps.CodeAppDraft, microsoftapps.CodeAppDisabled,
		microsoftapps.CodeAppNeedsAttention, microsoftapps.CodeTypeNotServed, microsoftapps.CodeProjectNotServed,
		microsoftapps.CodeTenantMismatch, microsoftapps.CodeTenantRequired,
	} {
		describer := &fakeDescriber{err: &microsoftapps.ResolveError{Code: code, Message: "An administrator must look at the app."}}
		handler, requested := appHandler(t, describer, goodEntraDiscovery())
		// The credential has a perfectly good client of its own: it must not be used.
		recorder := postDelegatedCheck(t, handler, "outlook", encode(t, map[string]any{
			"oauth_app_id": testAppID, "client_id": "app-id", "client_secret": plantedSecret, "oauth_discovery_endpoint": entraTenant,
		}))
		body := recorder.Body.String()
		if recorder.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+code+`"`) || !strings.Contains(body, "auth_failed") {
			t.Errorf("%s: status %d body %s", code, recorder.Code, body)
		}
		if len(*requested) != 0 {
			t.Errorf("%s: fetched %v", code, *requested)
		}
	}
}

func TestARegistryFailureIsReportedWithoutItsDetail(t *testing.T) {
	describer := &fakeDescriber{err: errors.New("pq: password authentication failed for user registry_admin")}
	handler, requested := appHandler(t, describer, goodEntraDiscovery())
	recorder := postDelegatedCheck(t, handler, "outlook", encode(t, map[string]any{"oauth_app_id": testAppID}))
	if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "password") || !strings.Contains(recorder.Body.String(), "could not be read") {
		t.Errorf("status %d body %s", recorder.Code, recorder.Body.String())
	}
	if len(*requested) != 0 {
		t.Errorf("fetched %v", *requested)
	}
}

func TestADefaultAppOnATypeWithoutTheModeIsNotResolved(t *testing.T) {
	// Every Microsoft descriptor supports Default apps today; a type the descriptors do not
	// know never reaches the registry at all.
	describer := &fakeDescriber{resolved: resolvedApp(entraTenant, true, nil)}
	handler, _ := appHandler(t, describer, goodEntraDiscovery())
	postDelegatedCheck(t, handler, "github", encode(t, map[string]any{"oauth_app_id": testAppID}))
	if len(describer.asked) != 0 {
		t.Errorf("the registry was asked for a non-Microsoft type: %+v", describer.asked)
	}
}
