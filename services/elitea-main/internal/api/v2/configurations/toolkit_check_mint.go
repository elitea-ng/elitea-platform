// toolkit_check_mint.go is the two-step probe: mint a short-lived token from
// the stored client credentials, spend it on at most one GET, drop it.
//
// The GitHub App probe does the same in spirit — it signs a JWT with the stored
// key and spends it on `GET /app` — but signs locally. Xray Cloud and an Entra
// app-only SharePoint credential cannot: the provider mints the token, so the
// probe makes one extra round trip, and that round trip is where a wrong client
// id or secret is refused.
//
// The token never leaves this function. It is not logged, not returned, not
// stored, and the Message of every outcome is one of the fixed constants of
// toolkit_check.go.
package configurations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// mintResponseLimit bounds what is read of a token answer. A token response is
// a few hundred bytes; anything larger is not one.
const mintResponseLimit = 64 << 10

// mintRequest is the token POST.
type mintRequest struct {
	url         string
	contentType string
	body        []byte
	// basicUser and basicPass, when basicAuth is set, ride as an HTTP Basic
	// Authorization header (the OpenAPI credential's `method: Basic`).
	basicAuth bool
	basicUser string
	basicPass string
}

// tokenMint describes a probe that mints before it asks.
type tokenMint struct {
	// build turns the stored data into the token request, with the same three
	// answers an authorize step gives: build it, say the method is unsupported,
	// or reject material this probe understood and cannot use.
	build func(data map[string]any) (mintRequest, authorizeOutcome)
	// spend names the GET the minted token is spent on. Nil means the mint is
	// itself the verdict (Xray: the authenticate call is the credential check).
	spend func(data map[string]any) (string, bool)
	// mintIsVerdict, when set and true for the stored data, makes the mint
	// the verdict although spend is set: a SharePoint credential with a
	// tenant and no site has nothing to read, and a minted token is the
	// proof (#2251).
	mintIsVerdict func(data map[string]any) bool
}

// checkMinted runs a mint-then-spend probe.
func (c *httpToolkitConnectionChecker) checkMinted(ctx context.Context, configType string, mint *tokenMint, data map[string]any) ToolkitCheckOutcome {
	minted, auth := mint.build(data)
	if !auth.authorized {
		return unauthorizedOutcome(auth)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, minted.url, bytes.NewReader(minted.body))
	if err != nil {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnreachable, Message: toolkitCheckUnreachableMessage}
	}
	request.Header.Set("Content-Type", minted.contentType)
	request.Header.Set("Accept", "application/json")
	if minted.basicAuth {
		setBasicAuth(request.Header, minted.basicUser, minted.basicPass)
	}

	response, refused := c.send(ctx, configType, request)
	if refused != nil {
		return *refused
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// A token endpoint answers a wrong secret with 400 as readily as 401
		// (Entra's invalid_client). A 400 is a verdict on the credential only
		// when the OAuth error code says so: a wrong tenant or a malformed
		// base URL is a 400 too, and says nothing about the secret.
		if response.StatusCode == http.StatusBadRequest && mintErrorRefusesCredential(response.Body) {
			return ToolkitCheckOutcome{Reason: ToolkitCheckReasonAuthFailed, Message: toolkitCheckAuthFailedMessage}
		}
		return outcomeForStatus(response.StatusCode)
	}
	token := readMintedToken(response.Body)
	if token == "" {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnreachable, Message: toolkitCheckUnreachableMessage}
	}
	if mint.spend == nil || (mint.mintIsVerdict != nil && mint.mintIsVerdict(data)) {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonOK, Message: toolkitCheckOKMessage}
	}

	spendURL, ok := mint.spend(data)
	if !ok {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnsupportedType, Message: toolkitCheckUnsupportedAuthMessage}
	}
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, spendURL, nil)
	if err != nil {
		return ToolkitCheckOutcome{Reason: ToolkitCheckReasonUnreachable, Message: toolkitCheckUnreachableMessage}
	}
	get.Header.Set("Accept", "application/json")
	get.Header.Set("Authorization", "Bearer "+token)
	spent, refused := c.send(ctx, configType, get)
	if refused != nil {
		return *refused
	}
	defer func() { _ = spent.Body.Close() }()
	if spent.StatusCode == http.StatusForbidden || spent.StatusCode == http.StatusNotFound {
		// The provider just minted this token, so the secret is good. A 403
		// or 404 on the site read is the app's permission or the site's
		// address, which is not a verdict on the credential.
		return ToolkitCheckOutcome{
			Reason:  ToolkitCheckReasonUnreachable,
			Message: fmt.Sprintf("Could not verify the credential: the provider answered %d.", spent.StatusCode),
		}
	}
	return outcomeForStatus(spent.StatusCode)
}

// mintErrorRefusesCredential reads the OAuth `error` code of a failed token
// answer. These are the codes that mean the client id or secret was refused.
func mintErrorRefusesCredential(body io.Reader) bool {
	raw, err := io.ReadAll(io.LimitReader(body, mintResponseLimit))
	if err != nil {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return false
	}
	switch strVal(decoded, "error") {
	case "invalid_client", "unauthorized_client", "invalid_grant":
		return true
	}
	return false
}

// readMintedToken reads either shape a token endpoint answers with: an object
// carrying `access_token` (OAuth 2.0, Entra) or a bare JSON string (Xray).
func readMintedToken(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, mintResponseLimit))
	if err != nil {
		return ""
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return ""
	}
	switch value := decoded.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		return strings.TrimSpace(strVal(value, "access_token"))
	}
	return ""
}

// The endpoints of the Entra client-credentials probe. Variables, not
// constants, only so the tests can point them at an httptest provider through
// export_test.go; nothing else assigns them.
var (
	entraLoginBase = "https://login.microsoftonline.com"
	graphBase      = "https://graph.microsoft.com"
)

const graphDefaultScope = "https://graph.microsoft.com/.default"

// buildXrayMint is Xray Cloud's authenticate call. The base URL is the
// credential's own (regional hosts differ); the vendor's global host is the
// default.
func buildXrayMint(data map[string]any) (mintRequest, authorizeOutcome) {
	clientID := strings.TrimSpace(strVal(data, "client_id"))
	clientSecret := strings.TrimSpace(strVal(data, "client_secret"))
	if clientID == "" || clientSecret == "" {
		return mintRequest{}, unsupportedAuth()
	}
	base := strings.TrimRight(strings.TrimSpace(firstStrVal(data, "base_url", "url")), "/")
	if base == "" {
		base = "https://xray.cloud.getxray.app"
	}
	body, err := json.Marshal(map[string]string{"client_id": clientID, "client_secret": clientSecret})
	if err != nil {
		return mintRequest{}, unsupportedAuth()
	}
	return mintRequest{url: base + "/api/v2/authenticate", contentType: "application/json", body: body}, authorizedRequest()
}

// sharepointSite reads the site the credential names and the Entra tenant its
// host implies. The tenant is the host's first label (`contoso` for
// contoso.sharepoint.com), minus the `-my` / `-admin` suffixes of the personal
// and admin hosts, which is how the SDK derives it too.
func sharepointSite(data map[string]any) (site *url.URL, tenant string, ok bool) {
	raw := strings.TrimSpace(strVal(data, "site_url"))
	if raw == "" {
		return nil, "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, "", false
	}
	host := strings.ToLower(parsed.Hostname())
	label, rest, found := strings.Cut(host, ".")
	if !found || rest != "sharepoint.com" || label == "" {
		return nil, "", false
	}
	label = strings.TrimSuffix(strings.TrimSuffix(label, "-admin"), "-my")
	if label == "" {
		return nil, "", false
	}
	return parsed, label, true
}

// entraTenantPattern is what an explicit tenant may be: a tenant GUID or a
// domain name (`contoso.onmicrosoft.com`, `contoso.com`). It is one path
// segment of the token URL, so nothing else (a slash, a dot-dot, a scheme) is
// let through.
var entraTenantPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// sharepointExplicitTenant reads the tenant the credential names itself
// (`tenant_id`), which lets the check run without a site (#2251).
// An empty answer means none is named; ok is false for a value that is not a
// tenant, so a malformed one is unsupported rather than sent.
func sharepointExplicitTenant(data map[string]any) (tenant string, named, ok bool) {
	raw := strings.TrimSpace(strVal(data, "tenant_id"))
	if raw == "" {
		return "", false, true
	}
	if !entraTenantPattern.MatchString(raw) || strings.Contains(raw, "..") {
		return "", true, false
	}
	// The alias tenants cannot mint an app-only token: a valid app would be
	// reported as refused.
	switch strings.ToLower(raw) {
	case "common", "organizations", "consumers":
		return "", true, false
	}
	return raw, true, true
}

// sharepointSkipsSpend is true when the credential names a tenant and no site:
// the minted token is the whole proof.
func sharepointSkipsSpend(data map[string]any) bool {
	if _, named, ok := sharepointExplicitTenant(data); !named || !ok {
		return false
	}
	return strings.TrimSpace(strVal(data, "site_url")) == ""
}

// buildSharepointMint is the Entra client-credentials grant against the Graph
// default scope. The tenant is the credential's explicit one when it names
// one, and otherwise the one the site host implies. A credential that carries a discovery endpoint is the
// delegated flow and has no app-only secret to prove, so it is not probed here.
func buildSharepointMint(data map[string]any) (mintRequest, authorizeOutcome) {
	if strVal(data, "oauth_discovery_endpoint") != "" || strVal(data, "auth_mode") != "" || strVal(data, "oauth_app_id") != "" {
		return mintRequest{}, unsupportedAuth()
	}
	clientID := strings.TrimSpace(strVal(data, "client_id"))
	clientSecret := strings.TrimSpace(strVal(data, "client_secret"))
	tenant, named, tenantOK := sharepointExplicitTenant(data)
	if !tenantOK {
		return mintRequest{}, unsupportedAuth()
	}
	siteOK := true
	if !named {
		_, tenant, siteOK = sharepointSite(data)
		tenant += ".onmicrosoft.com"
	}
	if clientID == "" || clientSecret == "" || !siteOK {
		return mintRequest{}, unsupportedAuth()
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("scope", graphDefaultScope)
	return mintRequest{
		url:         entraLoginBase + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token",
		contentType: "application/x-www-form-urlencoded",
		body:        []byte(form.Encode()),
	}, authorizedRequest()
}

// sharepointSpendURL is the one Graph read the token is spent on: the site the
// credential names, which is the SDK's own call.
func sharepointSpendURL(data map[string]any) (string, bool) {
	site, _, ok := sharepointSite(data)
	if !ok {
		return "", false
	}
	path := strings.TrimRight(site.EscapedPath(), "/")
	if path == "" {
		// The tenant's root site: Graph addresses it by host alone.
		return graphBase + "/v1.0/sites/" + site.Host, true
	}
	return graphBase + "/v1.0/sites/" + site.Host + ":" + path, true
}

// buildOpenAPIMint is the OAuth 2.0 client-credentials grant of an OpenAPI
// credential, against the token URL the tenant typed (#2252), as elitea-sdk's
// `OpenApiConfiguration.check_connection` makes it. It is the only honest
// probe an OpenAPI credential has: a custom spec has no identity endpoint, so
// API-key and anonymous credentials stay `unsupported_type`, as does the
// delegated flow (an `oauth_discovery_endpoint`), which has no app-only secret.
//
// `method` picks where the client authenticates: `Basic` sends it in an HTTP
// Basic header (Spotify and others), anything else in the form body. The token
// URL must be https; whether its host may be dialled is the egress allowlist's
// say (`ELITEA_TOOLKIT_CHECK_ALLOWLIST`), which `send` applies as for every
// probe, so a host the operator has not named answers `unreachable`.
// openAPITokenScheme is the scheme a token URL must have. A variable, not a
// constant, only so the tests can serve a token endpoint over plain HTTP
// through export_test.go; nothing else assigns it.
var openAPITokenScheme = "https"

func buildOpenAPIMint(data map[string]any) (mintRequest, authorizeOutcome) {
	if strings.TrimSpace(strVal(data, "oauth_discovery_endpoint")) != "" {
		return mintRequest{}, unsupportedAuth()
	}
	clientID := strings.TrimSpace(strVal(data, "client_id"))
	clientSecret := strings.TrimSpace(strVal(data, "client_secret"))
	tokenURL := strings.TrimSpace(strVal(data, "token_url"))
	if clientID == "" || clientSecret == "" || tokenURL == "" {
		return mintRequest{}, unsupportedAuth()
	}
	parsed, err := url.Parse(tokenURL)
	if err != nil || parsed.Scheme != openAPITokenScheme || parsed.Hostname() == "" || parsed.User != nil {
		return mintRequest{}, unsupportedAuth()
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if scope := strings.TrimSpace(strVal(data, "scope")); scope != "" {
		form.Set("scope", scope)
	}
	request := mintRequest{url: parsed.String(), contentType: "application/x-www-form-urlencoded"}
	if strings.EqualFold(strings.TrimSpace(strVal(data, "method")), "basic") {
		// RFC 6749 section 2.3.1: both are form-urlencoded before the header.
		request.basicAuth, request.basicUser, request.basicPass = true, url.QueryEscape(clientID), url.QueryEscape(clientSecret)
	} else {
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}
	request.body = []byte(form.Encode())
	return request, authorizedRequest()
}
