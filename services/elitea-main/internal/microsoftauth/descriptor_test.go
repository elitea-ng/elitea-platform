package microsoftauth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestTheFiveTypesAreDescribed(t *testing.T) {
	want := []string{Outlook, Teams, MSGraph, Dataverse, SharePoint}
	if got := Types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Types = %v, want %v", got, want)
	}
	for _, name := range want {
		descriptor, ok := For(name)
		if !ok || descriptor.Type() != name || descriptor.Label() == "" || descriptor.GovernedAdapter() != name {
			t.Errorf("%s: descriptor = %+v ok=%v", name, descriptor, ok)
		}
	}
	if _, ok := For("github"); ok {
		t.Error("github is not a Microsoft type")
	}
}

func TestDescriptorsMatchTheADRTable(t *testing.T) {
	cases := []struct {
		typ      string
		audience Audience
		model    ScopeModel
		probe    Probe
		entra    bool
		app      bool
	}{
		{Outlook, AudienceGraph, ScopesFixedList, ProbeGraphMe, true, true},
		{Teams, AudienceGraph, ScopesFixedList, ProbeGraphMe, true, true},
		{MSGraph, AudienceGraph, ScopesFreeText, ProbeGraphMe, true, true},
		{Dataverse, AudienceOrg, ScopesOrgImpersonation, ProbeWhoAmI, true, true},
		{SharePoint, AudienceGraph, ScopesAsStored, ProbeNone, false, false},
	}
	for _, tc := range cases {
		d, _ := For(tc.typ)
		if d.Audience() != tc.audience || d.ScopeModel() != tc.model || d.Probe() != tc.probe ||
			d.EntraOnly() != tc.entra || d.DefaultAppSupported() != tc.app {
			t.Errorf("%s: %+v", tc.typ, d)
		}
	}
}

func TestSharePointIsDelegatedOnlyWithADiscoveryEndpoint(t *testing.T) {
	sharepoint, _ := For(SharePoint)
	if sharepoint.IsDelegated(map[string]any{"client_id": "a", "site_url": "https://x.sharepoint.com"}) {
		t.Error("an app-only SharePoint credential is not delegated")
	}
	if sharepoint.IsDelegated(map[string]any{FieldDiscoveryEndpoint: "  "}) {
		t.Error("a blank endpoint is not delegated")
	}
	if !sharepoint.IsDelegated(map[string]any{FieldDiscoveryEndpoint: "https://login.example.test/t"}) {
		t.Error("a SharePoint credential with an endpoint is delegated")
	}
	for _, name := range []string{Outlook, Teams, MSGraph, Dataverse} {
		d, _ := For(name)
		if !d.IsDelegated(map[string]any{}) {
			t.Errorf("%s is always delegated", name)
		}
	}
}

func TestAuthorityPerType(t *testing.T) {
	outlook, _ := For(Outlook)
	dataverse, _ := For(Dataverse)
	sharepoint, _ := For(SharePoint)

	if got, err := outlook.Authority(map[string]any{FieldDiscoveryEndpoint: "https://login.microsoftonline.com/contoso.com/"}); err != nil ||
		got != "https://login.microsoftonline.com/contoso.com" {
		t.Errorf("outlook authority = %q, %v", got, err)
	}
	if _, err := outlook.Authority(map[string]any{}); !errors.Is(err, ErrAuthorityMissing) {
		t.Errorf("an empty endpoint = %v, want ErrAuthorityMissing", err)
	}
	if _, err := outlook.Authority(map[string]any{FieldDiscoveryEndpoint: "https://login.example.test/t"}); !errors.Is(err, ErrNotEntraAuthority) {
		t.Errorf("a non-Entra endpoint = %v, want ErrNotEntraAuthority", err)
	}

	// Dataverse builds it from tenant_id; it has no discovery endpoint, and one
	// it carries is ignored.
	if got, err := dataverse.Authority(map[string]any{
		FieldTenantID: "11111111-2222-3333-4444-555555555555", FieldDiscoveryEndpoint: "https://evil.test",
	}); err != nil || got != "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555" {
		t.Errorf("dataverse authority = %q, %v", got, err)
	}
	for _, tenant := range []string{"", "a/b", "../x", "evil.test/path", "a b", ".x", "a@b", "t?x=1"} {
		if _, err := dataverse.Authority(map[string]any{FieldTenantID: tenant}); !errors.Is(err, ErrInvalidTenant) {
			t.Errorf("tenant %q = %v, want ErrInvalidTenant", tenant, err)
		}
	}

	// SharePoint keeps today's behaviour: the endpoint as written.
	if got, err := sharepoint.Authority(map[string]any{FieldDiscoveryEndpoint: "https://login.example.test/t"}); err != nil ||
		got != "https://login.example.test/t" {
		t.Errorf("sharepoint authority = %q, %v", got, err)
	}
}

func TestEntraAuthorityAcceptsOnlyLoginMicrosoftonline(t *testing.T) {
	good := map[string]string{
		"https://login.microsoftonline.com/contoso.onmicrosoft.com":              "https://login.microsoftonline.com/contoso.onmicrosoft.com",
		"https://login.microsoftonline.com/contoso.onmicrosoft.com/":             "https://login.microsoftonline.com/contoso.onmicrosoft.com",
		"https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0":         "https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0",
		"https://LOGIN.microsoftonline.com:443/organizations":                    "https://login.microsoftonline.com/organizations",
		"  https://login.microsoftonline.com/common  ":                           "https://login.microsoftonline.com/common",
		"https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555": "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555",
	}
	for raw, want := range good {
		if got, err := NormalizeEntraAuthority(raw); err != nil || got != want {
			t.Errorf("%q = %q, %v, want %q", raw, got, err, want)
		}
	}
	hostile := []string{
		"", "login.microsoftonline.com/t", "http://login.microsoftonline.com/t",
		"https://login.microsoftonline.com", "https://login.microsoftonline.com/",
		"https://login.microsoftonline.com.evil.test/t", "https://evil.test/login.microsoftonline.com/t",
		"https://login.microsoftonline.com@evil.test/t", "https://evil.test@login.microsoftonline.com/t",
		"https://login.microsoftonline.com:8443/t", "https://login.microsoftonline.com./t",
		"https://login.microsoftonline.us/t", "https://login.partner.microsoftonline.cn/t",
		"https://login.microsoftonline.com/t/extra", "https://login.microsoftonline.com/t/v1.0",
		"https://login.microsoftonline.com/t?x=1", "https://login.microsoftonline.com/t#frag",
		"https://login.microsoftonline.com/t?", "https://login.microsoftonline.com//t",
		"https://login.microsoftonline.com/../t", "https://127.0.0.1/t", "javascript:alert(1)",
		"https://login.microsoftonline.com/%2e%2e", "https://login.microsoftonline.com/a%2fb",
	}
	for _, raw := range hostile {
		if got, err := NormalizeEntraAuthority(raw); !errors.Is(err, ErrNotEntraAuthority) {
			t.Errorf("%q = %q, %v, want ErrNotEntraAuthority", raw, got, err)
		}
	}
}

func TestEntraEndpointCheckForADiscoveryDocument(t *testing.T) {
	if err := RequireEntraEndpoint("https://login.microsoftonline.com/t/oauth2/v2.0/authorize"); err != nil {
		t.Errorf("a real endpoint was refused: %v", err)
	}
	for _, bad := range []any{nil, 5, "", "https://evil.test/authorize", "http://login.microsoftonline.com/x",
		"https://login.microsoftonline.com.evil.test/x", "https://login.microsoftonline.com:444/x"} {
		if err := RequireEntraEndpoint(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if got := TokenEndpoint("https://login.microsoftonline.com/t/v2.0"); got != "https://login.microsoftonline.com/t/oauth2/v2.0/token" {
		t.Errorf("TokenEndpoint = %s", got)
	}
	if got := TokenEndpoint("https://login.microsoftonline.com/t"); got != "https://login.microsoftonline.com/t/oauth2/v2.0/token" {
		t.Errorf("TokenEndpoint = %s", got)
	}
}

func TestDataverseURLIsPinned(t *testing.T) {
	good := map[string]string{
		"https://org.crm4.dynamics.com":    "https://org.crm4.dynamics.com",
		"https://ORG.crm.dynamics.com/":    "https://org.crm.dynamics.com",
		"https://org.crm.dynamics.com:443": "https://org.crm.dynamics.com",
		"  https://a.b.c.dynamics.com  ":   "https://a.b.c.dynamics.com",
	}
	for raw, want := range good {
		if got, err := NormalizeDataverseURL(raw); err != nil || got != want {
			t.Errorf("%q = %q, %v, want %q", raw, got, err, want)
		}
	}
	bad := []string{
		"", "org.crm.dynamics.com", "http://org.crm.dynamics.com", "https://dynamics.com", "https://.dynamics.com",
		"https://org.crm.dynamics.cn", "https://org.crm.microsoftdynamics.us", "https://org.crm.dynamics.com.evil.test",
		"https://evil.test/org.crm.dynamics.com", "https://user@org.crm.dynamics.com", "https://org.crm.dynamics.com:8443",
		"https://org.crm.dynamics.com/api/data", "https://org.crm.dynamics.com?x=1", "https://org.crm.dynamics.com#x",
		"https://evil.test@org.crm.dynamics.com.evil.test", "https://127.0.0.1", "https://localhost", "https://org..crm.dynamics.com",
		"https://org.crm.dynamics.com:@evil.test",
	}
	for _, raw := range bad {
		if got, err := NormalizeDataverseURL(raw); !errors.Is(err, ErrDataverseURL) {
			t.Errorf("%q = %q, %v, want ErrDataverseURL", raw, got, err)
		}
	}
}

func TestScopesPerType(t *testing.T) {
	outlook, _ := For(Outlook)
	msgraph, _ := For(MSGraph)
	dataverse, _ := For(Dataverse)
	sharepoint, _ := For(SharePoint)

	got, dropped, err := outlook.Scopes(map[string]any{FieldScopes: []any{"mail.send", "Files.Read"}})
	if err != nil || !reflect.DeepEqual(got, []string{"Mail.Send"}) || !reflect.DeepEqual(dropped, []string{"Files.Read"}) {
		t.Errorf("outlook = %v %v %v", got, dropped, err)
	}
	if got, _, _ := outlook.Scopes(map[string]any{}); !reflect.DeepEqual(got, []string{"Mail.Read"}) {
		t.Errorf("outlook default = %v", got)
	}
	if _, _, err := msgraph.Scopes(map[string]any{FieldScopes: "https://outlook.office.com/x.y"}); !errors.Is(err, ErrScopes) {
		t.Errorf("ms_graph foreign resource = %v, want ErrScopes", err)
	}
	if got, _, err := dataverse.Scopes(map[string]any{FieldURL: "https://org.crm4.dynamics.com/", FieldScopes: []any{"ignored"}}); err != nil ||
		!reflect.DeepEqual(got, []string{"https://org.crm4.dynamics.com/user_impersonation"}) {
		t.Errorf("dataverse = %v, %v", got, err)
	}
	if _, _, err := dataverse.Scopes(map[string]any{FieldURL: "https://evil.test"}); !errors.Is(err, ErrDataverseURL) {
		t.Errorf("dataverse bad host = %v", err)
	}
	if got, _, _ := sharepoint.Scopes(map[string]any{FieldScopes: []any{"Sites.Read.All", 4}}); !reflect.DeepEqual(got, []string{"Sites.Read.All"}) {
		t.Errorf("sharepoint = %v", got)
	}
}

func TestSignInScopesHonourTheRefreshSwitch(t *testing.T) {
	outlook, _ := For(Outlook)
	dataverse, _ := For(Dataverse)
	sharepoint, _ := For(SharePoint)

	if got, _, _ := outlook.SignInScopes(map[string]any{FieldScopes: []any{"Mail.Send"}}); !reflect.DeepEqual(got, []string{"offline_access", "Mail.Send"}) {
		t.Errorf("default = %v", got)
	}
	if got, _, _ := outlook.SignInScopes(map[string]any{FieldScopes: []any{"Mail.Send"}, FieldAutoRefresh: false}); !reflect.DeepEqual(got, []string{"Mail.Send"}) {
		t.Errorf("refresh off = %v", got)
	}
	if got, _, _ := dataverse.SignInScopes(map[string]any{FieldURL: "https://o.crm.dynamics.com"}); !reflect.DeepEqual(got,
		[]string{"offline_access", "https://o.crm.dynamics.com/user_impersonation"}) {
		t.Errorf("dataverse = %v", got)
	}
	// SharePoint's sign-in scopes are unchanged: what the credential lists.
	if got, _, _ := sharepoint.SignInScopes(map[string]any{FieldScopes: []any{"Sites.Read.All"}}); !reflect.DeepEqual(got, []string{"Sites.Read.All"}) {
		t.Errorf("sharepoint = %v", got)
	}
}

func TestAudience(t *testing.T) {
	outlook, _ := For(Outlook)
	dataverse, _ := For(Dataverse)
	if got, err := outlook.AudienceURL(nil); err != nil || got != "https://graph.microsoft.com" {
		t.Errorf("outlook = %q, %v", got, err)
	}
	if got, err := dataverse.AudienceURL(map[string]any{FieldURL: "https://o.crm.dynamics.com/"}); err != nil || got != "https://o.crm.dynamics.com" {
		t.Errorf("dataverse = %q, %v", got, err)
	}
}

func TestIdentityProbeRequests(t *testing.T) {
	ctx := context.Background()
	outlook, _ := For(Outlook)
	request, err := outlook.ProbeRequest(ctx, nil, "tok")
	if err != nil || request.Method != http.MethodGet || request.URL.Host != "graph.microsoft.com" ||
		request.URL.Path != "/v1.0/me" || request.Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("graph probe = %+v, %v", request, err)
	}
	dataverse, _ := For(Dataverse)
	request, err = dataverse.ProbeRequest(ctx, map[string]any{FieldURL: "https://o.crm4.dynamics.com"}, "tok")
	if err != nil || request.URL.String() != "https://o.crm4.dynamics.com/api/data/v9.2/WhoAmI" ||
		request.Header.Get("OData-Version") != "4.0" {
		t.Fatalf("dataverse probe = %+v, %v", request, err)
	}
	// A bad Dataverse host never gets a request, so the token goes nowhere.
	if request, err := dataverse.ProbeRequest(ctx, map[string]any{FieldURL: "https://evil.test"}, "tok"); err == nil || request != nil {
		t.Fatalf("a bad host built a request: %+v, %v", request, err)
	}
	sharepoint, _ := For(SharePoint)
	if request, err := sharepoint.ProbeRequest(ctx, nil, "tok"); request != nil || err != nil {
		t.Fatalf("sharepoint has no probe here: %+v, %v", request, err)
	}
}

func TestClassifyProbe(t *testing.T) {
	for status, want := range map[int]ProbeResult{
		200: ProbeOK, 401: ProbeUnauthorized, 403: ProbeForbidden, 429: ProbeFailed, 500: ProbeFailed,
		503: ProbeFailed, 404: ProbeFailed, 204: ProbeFailed,
	} {
		if got := ClassifyProbe(status); got != want {
			t.Errorf("status %d = %v, want %v", status, got, want)
		}
	}
}
