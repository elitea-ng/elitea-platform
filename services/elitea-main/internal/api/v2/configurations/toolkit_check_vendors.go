// toolkit_check_vendors.go carries the probes for the project-management and
// test-management families added for #2143: ado, qtest, testrail, xray,
// zephyr_enterprise, zephyr_essential, sharepoint, report_portal and testio.
// Each is a port of the `check_connection` of the matching class in
// elitea-sdk (elitea_sdk/configurations/<type>.py), restated under this
// package's rules: one bounded request to the endpoint the stored credential
// names, the egress allowlist first, the closed four-reason vocabulary, and a
// fixed message with no provider text in it.
//
// Two places this file departs from the SDK on purpose, because the SDK's
// answer there certifies nothing:
//
//   - A credential with NO secret (an Azure DevOps or Zephyr Enterprise row
//     without a token) is `unsupported_type`, not `ok`. The SDK reads an
//     anonymous healthcheck for those and reports success, which says the host
//     is up and nothing about the credential.
//   - A SharePoint credential that names no site (the normal case since the
//     site moved onto the toolkit) is `unsupported_type`. The SDK returns
//     success without making a request; the Entra tenant this probe signs in
//     to is derived from the site host, so without one there is nothing to ask.
//
// `openapi` is deliberately NOT here. A custom OpenAPI spec has no identity
// endpoint: the SDK itself skips validation for anonymous and API-key
// credentials and only exchanges a token for an OAuth client-credentials one,
// against a token URL the tenant typed. There is no honest probe for the
// common shapes, so it keeps the `unsupported_type` answer.
package configurations

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// vendorProbes returns the probes of this file, merged into toolkitCheckProbes
// by its initialiser. A function rather than a literal in the main map so the
// per-vendor commentary stays beside the code it explains.
func vendorProbes() map[string]toolkitProbe {
	return map[string]toolkitProbe{
		// ado: the organisation's projects read, one row. The SDK chose it
		// because it needs authentication whatever a project's visibility; the
		// PAT rides as the basic-auth PASSWORD with an empty user name.
		"ado": {
			baseURLFields: []string{"organization_url", "base_url", "url"},
			path:          func(map[string]any) string { return "/_apis/projects?api-version=7.1&$top=1" },
			authorize:     authorizeBasicEmptyUser("token", "pat", "api_key"),
			// Azure DevOps answers a PAT it does not recognise with 203 and a
			// sign-in page rather than 401.
			authFailedStatuses: []int{http.StatusNonAuthoritativeInfo},
		},
		// qtest: the projects read, one row. A pasted `/api/v3` suffix is
		// stripped, as the SDK does, so the canonical path is not doubled.
		"qtest": {
			baseURLFields: []string{"base_url", "url"},
			normalizeBase: stripQtestAPIVersion,
			path:          func(map[string]any) string { return "/api/v3/projects?pageSize=1&page=1" },
			authorize:     authorizeBearer("qtest_api_token", "api_token", "token"),
		},
		// testrail: the user the credential names, by e-mail. The e-mail is
		// the credential's own, so it is escaped into the query, never trusted.
		"testrail": {
			baseURLFields: []string{"url", "base_url"},
			path: func(data map[string]any) string {
				return "/index.php?/api/v2/get_user_by_email&email=" + url.QueryEscape(strings.TrimSpace(strVal(data, "email")))
			},
			authorize: authorizeBasicPair("email", "password"),
		},
		// zephyr_enterprise: the current user. A row without a token is
		// unsupported (see the file comment).
		"zephyr_enterprise": {
			baseURLFields: []string{"base_url", "url"},
			path:          func(map[string]any) string { return "/flex/services/rest/latest/user/current" },
			authorize:     authorizeBearer("token", "api_key"),
		},
		// zephyr_essential (Zephyr Scale cloud): the default API host is the
		// vendor's own, as the SDK's prefill; the projects read is one row.
		"zephyr_essential": {
			baseURLFields:  []string{"base_url", "url"},
			defaultBaseURL: "https://api.zephyrscale.smartbear.com/v2",
			path:           func(map[string]any) string { return "/projects?maxResults=1" },
			authorize:      authorizeBearer("token", "api_key"),
		},
		// report_portal: the project the credential names, which is also the
		// scope the API key is valid for, so a key without access to it is a
		// refused credential for this toolkit.
		"report_portal": {
			baseURLFields: []string{"endpoint", "base_url", "url"},
			path: func(data map[string]any) string {
				return "/api/v1/project/" + url.PathEscape(strings.TrimSpace(strVal(data, "project")))
			},
			authorize: authorizeReportPortal,
		},
		// testio: the products read. TestIO's own scheme is `Token <key>`,
		// which a bearer header would be answered 401 for.
		"testio": {
			baseURLFields: []string{"endpoint", "base_url", "url"},
			path:          func(map[string]any) string { return "/customer/v2/products" },
			authorize:     authorizeScheme("Token", "api_key", "token"),
		},
		// xray (Xray Cloud): the authenticate endpoint IS the identity check —
		// it exchanges the client id and secret for a token, and a pair it does
		// not recognise is refused. The token is minted, read, and dropped.
		// Xray's REST surface has no GET that names the caller, so, unlike
		// sharepoint, the minted token is not spent on a second request.
		"xray": {mint: &tokenMint{build: buildXrayMint}},
		// sharepoint (client credentials): an Entra app-only token is minted
		// with the stored client id and secret, spent on ONE Graph read of the
		// site the credential names, and dropped. Delegated and app-mode
		// credentials never reach this probe (check_connection_oauth.go).
		"sharepoint": {mint: &tokenMint{build: buildSharepointMint, spend: sharepointSpendURL}},
	}
}

var qtestAPIVersionSuffix = regexp.MustCompile(`(?i)/api/v\d+/?$`)

func stripQtestAPIVersion(base string) string {
	return qtestAPIVersionSuffix.ReplaceAllString(strings.TrimRight(base, "/"), "")
}

// authorizeBasicEmptyUser is HTTP basic with an empty user name and the secret
// as the password — Azure DevOps' personal-access-token shape.
func authorizeBasicEmptyUser(secretFields ...string) func(http.Header, map[string]any) authorizeOutcome {
	return func(header http.Header, data map[string]any) authorizeOutcome {
		secret := strings.TrimSpace(firstStrVal(data, secretFields...))
		if secret == "" {
			return unsupportedAuth()
		}
		setBasicAuth(header, "", secret)
		return authorizedRequest()
	}
}

// authorizeScheme presents the first non-empty field under a named
// Authorization scheme (`Token <key>`).
func authorizeScheme(scheme string, tokenFields ...string) func(http.Header, map[string]any) authorizeOutcome {
	return func(header http.Header, data map[string]any) authorizeOutcome {
		token := strings.TrimSpace(firstStrVal(data, tokenFields...))
		if token == "" {
			return unsupportedAuth()
		}
		header.Set("Authorization", scheme+" "+token)
		return authorizedRequest()
	}
}

// authorizeReportPortal needs the project as well as the key: the probe path
// is project-scoped, and an empty project would read the project LIST.
func authorizeReportPortal(header http.Header, data map[string]any) authorizeOutcome {
	if strings.TrimSpace(strVal(data, "project")) == "" {
		return unsupportedAuth()
	}
	return authorizeBearer("api_key", "token")(header, data)
}
