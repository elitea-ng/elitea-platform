package provideroauth

// Atlassian 3LO API base (#1464).
//
// Atlassian accepts a 3LO access token only at the API gateway, under
// /ex/<product>/<cloudid>, never at the site URL. At the consent callback
// Main reads the sites the new token can reach (accessible-resources),
// finds the one the target binding names, and stores
// <gateway>/ex/<product>/<cloudid> with the grant. The broker releases that
// base with the token, and the worker calls it. A token whose consent does
// not cover the target site stores no grant.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ErrSiteNotAccessible is a consent whose token cannot reach the target
// site: the user picked another site on the Atlassian consent page, or the
// app has no access to it.
var ErrSiteNotAccessible = errors.New("provideroauth: the consent does not cover the target site")

// accessibleResourcesPath is the Atlassian endpoint that lists the sites a
// 3LO token can reach.
const accessibleResourcesPath = "/oauth/token/accessible-resources"

const maxResourcesResponseBytes = 256 << 10

// cloudIDPattern is an Atlassian cloud id (a UUID today); anything else is
// refused before it becomes part of a URL.
var cloudIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// atlassianProducts maps a provider type to its API gateway product segment.
var atlassianProducts = map[string]string{"jira": "jira", "confluence": "confluence"}

// APIBaseResolver answers the API base a consented token must call. An
// authorizer that has no API gateway does not implement it.
type APIBaseResolver interface {
	// ResolveAPIBase answers the base for site, the target binding's
	// canonical identity. Empty means the provider has no gateway.
	ResolveAPIBase(ctx context.Context, accessToken, site string) (string, error)
}

var _ APIBaseResolver = (*Client)(nil)

// ResolveAPIBase implements APIBaseResolver. Only an Atlassian client has a
// gateway; any other flavor answers "".
func (c *Client) ResolveAPIBase(ctx context.Context, accessToken, site string) (string, error) {
	product, ok := atlassianProducts[c.cfg.Provider]
	if c.cfg.Flavor != FlavorAtlassian || !ok {
		return "", nil
	}
	want, err := siteOrigin(site)
	if err != nil {
		return "", ErrSiteNotAccessible
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIURL+accessibleResourcesPath, nil)
	if err != nil {
		return "", ErrProviderUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", ErrProviderUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResourcesResponseBytes+1))
	switch {
	case err != nil || len(raw) > maxResourcesResponseBytes || resp.StatusCode >= 500 ||
		resp.StatusCode == http.StatusTooManyRequests:
		return "", ErrProviderUnavailable
	case resp.StatusCode != http.StatusOK:
		return "", ErrProviderRejected
	}
	var resources []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &resources) != nil {
		return "", ErrProviderRejected
	}
	for _, resource := range resources {
		origin, err := siteOrigin(resource.URL)
		if err != nil || origin != want || !cloudIDPattern.MatchString(resource.ID) {
			continue
		}
		return c.apiBase(product, resource.ID), nil
	}
	return "", ErrSiteNotAccessible
}

func (c *Client) apiBase(product, cloudID string) string {
	return c.cfg.APIURL + "/ex/" + product + "/" + cloudID
}

// validAPIBase reports whether base is one this client could have stored:
// <gateway>/ex/<product>/<cloud id>. A stored value that is not is never
// released.
func (c *Client) validAPIBase(base string) bool {
	product, ok := atlassianProducts[c.cfg.Provider]
	if c.cfg.Flavor != FlavorAtlassian || !ok {
		return false
	}
	cloudID, ok := strings.CutPrefix(base, c.cfg.APIURL+"/ex/"+product+"/")
	return ok && cloudIDPattern.MatchString(cloudID)
}

// siteOrigin is the lowercased scheme://host[:port] of an Atlassian site
// URL. A Confluence target may carry the /wiki context path; the site is
// the same.
func siteOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname()))) {
		return "", errors.New("not a site URL")
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path != "" && path != "/wiki" {
		return "", errors.New("not a site URL")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}
