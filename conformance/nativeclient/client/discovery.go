package client

import (
	"context"
	"fmt"
	"net/http"
)

// DiscoveryPath is the one URL a native client derives from an origin alone.
const DiscoveryPath = "/.well-known/elitea-client"

// Discovery is the discovery document (ADR-0025 WP1).
type Discovery struct {
	ServerVersion  string       `json:"server_version"`
	ClientContract string       `json:"client_contract"`
	DeploymentKind string       `json:"deployment_kind"`
	DisplayName    string       `json:"display_name"`
	BrandPackURL   string       `json:"brand_pack_url"`
	NativeAuth     *NativeAuth  `json:"native_auth"`
	ClientPolicy   PublicPolicy `json:"client_policy"`
	// MinClientVersion is each registered client's effective minimum,
	// keyed by client id.
	MinClientVersion map[string]string `json:"min_client_version"`
}

// NativeAuth is the authorization server metadata; nil while no native client
// is registered on the deployment.
type NativeAuth struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RevocationEndpoint            string   `json:"revocation_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// PublicPolicy is the public part of the native client policy.
type PublicPolicy struct {
	RequireDeviceLock bool   `json:"require_device_lock"`
	OfflineEnabled    bool   `json:"offline_enabled"`
	MinClientVersion  string `json:"min_client_version"`
}

// DiscoveryResult is one fetch of the document.
type DiscoveryResult struct {
	Document    Discovery
	Raw         map[string]any
	ETag        string
	NotModified bool
	Response    *Response
}

// FetchDiscovery reads the discovery document; a non-empty etag is sent as
// If-None-Match and a 304 is reported as NotModified.
func (c *Client) FetchDiscovery(ctx context.Context, etag string) (DiscoveryResult, error) {
	header := http.Header{}
	if etag != "" {
		header.Set("If-None-Match", etag)
	}
	response, err := c.Do(ctx, http.MethodGet, DiscoveryPath, nil, header)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{ETag: response.Header.Get("ETag"), Response: response}
	switch response.Status {
	case http.StatusNotModified:
		result.NotModified = true
		return result, nil
	case http.StatusOK:
	default:
		return result, fmt.Errorf("discovery: %s", response)
	}
	if err := response.JSON(&result.Document); err != nil {
		return result, err
	}
	if err := response.JSON(&result.Raw); err != nil {
		return result, err
	}
	return result, nil
}
