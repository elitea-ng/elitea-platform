package guardrail

// The shared envelope of the self-hosted classifier adapters (ADR-0043
// decision 1): llama_guard (adapter_llamaguard.go) and presidio
// (adapter_presidio.go).
//
// The envelope is the one of the generic http adapter (httpadapter.go):
//
//   - The call is EGRESS. The URL must pass the gateway's egress allowlist. A
//     private address is reachable only when the allowlist names a private
//     destination.
//   - The client never follows a redirect (Env.HTTPClient).
//   - The auth value comes from a {{secret.NAME}} reference, never a raw value.
//   - The response body is bounded (maxAdapterResponseBytes).
//   - The row's timeout and fail mode apply (pipeline.go).
//   - An error message names the failure, never the content or the secret.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// selfHostedEndpoint is the URL and the auth of a self-hosted adapter.
type selfHostedEndpoint struct {
	URL string
	// AuthHeader carries the secret. AuthPrefix goes before the secret value
	// (for example "Bearer ") when the value does not already start with it.
	AuthHeader string
	AuthPrefix string
	// AuthSecret is a {{secret.NAME}} reference, or empty for no auth.
	AuthSecret string
}

// parseSelfHostedEndpoint reads url and auth_secret, the fields every
// self-hosted adapter shares.
func parseSelfHostedEndpoint(obj map[string]any, header, prefix string) (selfHostedEndpoint, error) {
	ep := selfHostedEndpoint{AuthHeader: header, AuthPrefix: prefix}
	ep.URL, _ = obj["url"].(string)
	u, err := url.Parse(ep.URL)
	if err != nil || ep.URL == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ep, errors.New("url must be an absolute http or https URL")
	}
	if u.User != nil {
		return ep, errors.New("url must not carry credentials; use auth_secret")
	}
	if s, present := obj["auth_secret"]; present && s != nil {
		ref, ok := s.(string)
		if !ok || !isSecretRef(ref) {
			return ep, errors.New("auth_secret must be a {{secret.NAME}} reference to the secrets vault, never a raw value")
		}
		ep.AuthSecret = ref
	}
	return ep, nil
}

// post sends body as JSON to the endpoint and returns the bounded response
// body of a 200 answer (egressPost).
func (ep selfHostedEndpoint) post(ctx context.Context, env Env, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", errAdapter)
	}
	return egressPost(ctx, env, ep.URL, payload, secretHeader(ctx, env, "auth_secret", ep.AuthSecret, ep.AuthHeader, ep.AuthPrefix))
}
