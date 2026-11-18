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
	if mint.spend == nil {
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

// buildSharepointMint is the Entra client-credentials grant against the Graph
// default scope. A credential that carries a discovery endpoint is the
// delegated flow and has no app-only secret to prove, so it is not probed here.
func buildSharepointMint(data map[string]any) (mintRequest, authorizeOutcome) {
	if strVal(data, "oauth_discovery_endpoint") != "" || strVal(data, "auth_mode") != "" || strVal(data, "oauth_app_id") != "" {
		return mintRequest{}, unsupportedAuth()
	}
	clientID := strings.TrimSpace(strVal(data, "client_id"))
	clientSecret := strings.TrimSpace(strVal(data, "client_secret"))
	_, tenant, ok := sharepointSite(data)
	if clientID == "" || clientSecret == "" || !ok {
		return mintRequest{}, unsupportedAuth()
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("scope", graphDefaultScope)
	return mintRequest{
		url:         entraLoginBase + "/" + url.PathEscape(tenant+".onmicrosoft.com") + "/oauth2/v2.0/token",
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
