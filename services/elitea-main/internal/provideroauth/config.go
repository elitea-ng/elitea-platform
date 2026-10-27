package provideroauth

import (
	"fmt"
	"strings"
	"time"
)

// The provider OAuth variables, one constant each so a search for a
// variable finds where it is read.
const (
	jiraClientIDEnv           = "ELITEA_PROVIDER_OAUTH_JIRA_CLIENT_ID"
	jiraClientSecretEnv       = "ELITEA_PROVIDER_OAUTH_JIRA_CLIENT_SECRET"
	jiraRedirectURIEnv        = "ELITEA_PROVIDER_OAUTH_JIRA_REDIRECT_URI"
	jiraAuthorizeURLEnv       = "ELITEA_PROVIDER_OAUTH_JIRA_AUTHORIZE_URL"
	jiraTokenURLEnv           = "ELITEA_PROVIDER_OAUTH_JIRA_TOKEN_URL"
	jiraScopesEnv             = "ELITEA_PROVIDER_OAUTH_JIRA_SCOPES"
	jiraTimeoutEnv            = "ELITEA_PROVIDER_OAUTH_JIRA_TIMEOUT"
	confluenceClientIDEnv     = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_CLIENT_ID"
	confluenceClientSecretEnv = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_CLIENT_SECRET"
	confluenceRedirectURIEnv  = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_REDIRECT_URI"
	confluenceAuthorizeURLEnv = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_AUTHORIZE_URL"
	confluenceTokenURLEnv     = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_TOKEN_URL"
	confluenceScopesEnv       = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_SCOPES"
	confluenceTimeoutEnv      = "ELITEA_PROVIDER_OAUTH_CONFLUENCE_TIMEOUT"
	ahaClientIDEnv            = "ELITEA_PROVIDER_OAUTH_AHA_CLIENT_ID"
	ahaClientSecretEnv        = "ELITEA_PROVIDER_OAUTH_AHA_CLIENT_SECRET"
	ahaRedirectURIEnv         = "ELITEA_PROVIDER_OAUTH_AHA_REDIRECT_URI"
	ahaAuthorizeURLEnv        = "ELITEA_PROVIDER_OAUTH_AHA_AUTHORIZE_URL"
	ahaTokenURLEnv            = "ELITEA_PROVIDER_OAUTH_AHA_TOKEN_URL"
	ahaScopesEnv              = "ELITEA_PROVIDER_OAUTH_AHA_SCOPES"
	ahaTimeoutEnv             = "ELITEA_PROVIDER_OAUTH_AHA_TIMEOUT"
)

// supported is every provider type with a provider OAuth application, and
// its token endpoint dialect. SharePoint is not here: its oauth_delegated
// class is the Entra on-behalf-of exchange.
var supported = []struct {
	provider string
	flavor   Flavor
	env      envNames
}{
	{"jira", FlavorAtlassian, envNames{
		clientID: jiraClientIDEnv, clientSecret: jiraClientSecretEnv,
		redirectURI: jiraRedirectURIEnv, authorizeURL: jiraAuthorizeURLEnv,
		tokenURL: jiraTokenURLEnv, scopes: jiraScopesEnv,
		timeout: jiraTimeoutEnv,
	}},
	{"confluence", FlavorAtlassian, envNames{
		clientID: confluenceClientIDEnv, clientSecret: confluenceClientSecretEnv,
		redirectURI: confluenceRedirectURIEnv, authorizeURL: confluenceAuthorizeURLEnv,
		tokenURL: confluenceTokenURLEnv, scopes: confluenceScopesEnv,
		timeout: confluenceTimeoutEnv,
	}},
	{"aha", FlavorAha, envNames{
		clientID: ahaClientIDEnv, clientSecret: ahaClientSecretEnv,
		redirectURI: ahaRedirectURIEnv, authorizeURL: ahaAuthorizeURLEnv,
		tokenURL: ahaTokenURLEnv, scopes: ahaScopesEnv,
		timeout: ahaTimeoutEnv,
	}},
}

// envNames are one provider's variables, spelled out so a search for a
// variable finds where it is read.
type envNames struct {
	clientID, clientSecret, redirectURI, authorizeURL, tokenURL, scopes, timeout string
}

// Default least-privilege scopes per provider; an operator may replace them.
var defaultScopes = map[string][]string{
	"jira":       {"read:jira-work", "write:jira-work", "read:jira-user", "offline_access"},
	"confluence": {"read:confluence-content.all", "write:confluence-content", "offline_access"},
}

// ConfigFromEnv reads the provider OAuth applications. A provider with no
// client id is not configured; one with a client id must have every other
// required value. Variables, per provider P in JIRA, CONFLUENCE and AHA:
//
//	ELITEA_PROVIDER_OAUTH_<P>_CLIENT_ID
//	ELITEA_PROVIDER_OAUTH_<P>_CLIENT_SECRET
//	ELITEA_PROVIDER_OAUTH_<P>_REDIRECT_URI    exact, …/api/v2/credential_consent/<p>:callback
//	ELITEA_PROVIDER_OAUTH_<P>_AUTHORIZE_URL   Atlassian default auth.atlassian.com; Aha! required
//	ELITEA_PROVIDER_OAUTH_<P>_TOKEN_URL       Atlassian default auth.atlassian.com; Aha! required
//	ELITEA_PROVIDER_OAUTH_<P>_SCOPES          space separated; Atlassian has defaults, Aha! uses none
//	ELITEA_PROVIDER_OAUTH_<P>_TIMEOUT         Go duration, default 10s
func ConfigFromEnv(lookup func(string) (string, bool)) ([]ProviderConfig, error) {
	get := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}
	var out []ProviderConfig
	for _, s := range supported {
		cfg := ProviderConfig{
			Provider:     s.provider,
			Flavor:       s.flavor,
			ClientID:     get(s.env.clientID),
			ClientSecret: get(s.env.clientSecret),
			RedirectURI:  get(s.env.redirectURI),
			AuthorizeURL: get(s.env.authorizeURL),
			TokenURL:     get(s.env.tokenURL),
			Scopes:       strings.Fields(get(s.env.scopes)),
		}
		if cfg.ClientID == "" {
			if cfg.ClientSecret != "" || cfg.RedirectURI != "" {
				return nil, fmt.Errorf("provider OAuth %s: %s is required when the other values are set", s.provider, s.env.clientID)
			}
			continue
		}
		if raw := get(s.env.timeout); raw != "" {
			timeout, err := time.ParseDuration(raw)
			if err != nil || timeout <= 0 || timeout > time.Minute {
				return nil, fmt.Errorf("provider OAuth %s: %s must be a duration up to 1m", s.provider, s.env.timeout)
			}
			cfg.Timeout = timeout
		}
		if s.flavor == FlavorAtlassian {
			if cfg.AuthorizeURL == "" {
				cfg.AuthorizeURL = AtlassianAuthorizeURL
			}
			if cfg.TokenURL == "" {
				cfg.TokenURL = AtlassianTokenURL
			}
			if len(cfg.Scopes) == 0 {
				cfg.Scopes = append([]string(nil), defaultScopes[s.provider]...)
			}
		}
		if err := cfg.validate(); err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, nil
}
