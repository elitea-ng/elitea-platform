package credentialadapters

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrInvalidTarget refuses a target that has no canonical form.
var ErrInvalidTarget = errors.New("credential adapter: invalid target")

const maxTargetBytes = 512

// CanonicalBaseURL canonicalizes a site base URL: https only, no userinfo,
// query or fragment; scheme and host lowercased, the default port dropped,
// trailing slashes trimmed from the path. "https://Acme.Atlassian.net/" is
// "https://acme.atlassian.net".
func CanonicalBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxTargetBytes || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", fmt.Errorf("%w: empty, too long or contains whitespace", ErrInvalidTarget)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: not a URL", ErrInvalidTarget)
	}
	switch {
	case !strings.EqualFold(u.Scheme, "https"):
		return "", fmt.Errorf("%w: the scheme must be https", ErrInvalidTarget)
	case u.User != nil:
		return "", fmt.Errorf("%w: userinfo is not allowed", ErrInvalidTarget)
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("%w: a query is not allowed", ErrInvalidTarget)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("%w: a fragment is not allowed", ErrInvalidTarget)
	case u.Opaque != "":
		return "", fmt.Errorf("%w: not a hierarchical URL", ErrInvalidTarget)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: a host is required", ErrInvalidTarget)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 literal
	}
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: dot segments are not allowed", ErrInvalidTarget)
		}
	}
	return "https://" + host + path, nil
}

// CanonicalTenantSite canonicalizes a tenant site URL
// ("https://contoso.sharepoint.com/sites/eng"). The rules are
// CanonicalBaseURL's, host required; the path names the site.
func CanonicalTenantSite(raw string) (string, error) { return CanonicalBaseURL(raw) }
