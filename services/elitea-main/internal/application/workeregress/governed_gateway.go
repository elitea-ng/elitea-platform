package workeregress

import "slices"

// GovernedOAuthGatewayURL is the API gateway a governed Atlassian 3LO token
// is used at (#1464): the worker calls
// https://api.atlassian.com/ex/<product>/<cloud id>, never the target site,
// with an OAUTH_ACCESS envelope. A governed run whose frozen credentials
// include an oauth_delegated reference of a provider below gets this host
// in its allowlist.
const GovernedOAuthGatewayURL = "https://api.atlassian.com"

// governedOAuthGatewayProviders are the provider types whose oauth_delegated
// token is used at GovernedOAuthGatewayURL.
var governedOAuthGatewayProviders = []string{"confluence", "jira"}

// GovernedOAuthGatewayProviders answers a copy of the provider types whose
// oauth_delegated token is used at GovernedOAuthGatewayURL.
func GovernedOAuthGatewayProviders() []string {
	return slices.Clone(governedOAuthGatewayProviders)
}
