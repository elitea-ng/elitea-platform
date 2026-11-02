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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
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
// body of a 200 answer.
func (ep selfHostedEndpoint) post(ctx context.Context, env Env, body any) ([]byte, error) {
	if env.HTTPClient == nil || env.EgressAllows == nil || !env.EgressAllows(ep.URL) {
		return nil, fmt.Errorf("%w: the adapter host is not on the egress allowlist", errAdapter)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", errAdapter)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", errAdapter)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if ep.AuthSecret != "" {
		if env.ResolveSecret == nil {
			return nil, fmt.Errorf("%w: no secrets vault is available for auth_secret", errAdapter)
		}
		value, rerr := env.ResolveSecret(ctx, ep.AuthSecret)
		if rerr != nil || value == "" {
			return nil, fmt.Errorf("%w: auth_secret does not resolve", errAdapter)
		}
		if ep.AuthPrefix != "" && !strings.HasPrefix(value, ep.AuthPrefix) {
			value = ep.AuthPrefix + value
		}
		req.Header.Set(ep.AuthHeader, value)
	}
	resp, err := env.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: timeout", errAdapter)
		}
		return nil, fmt.Errorf("%w: request failed", errAdapter)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAdapterResponseBytes))
		return nil, fmt.Errorf("%w: status %d", errAdapter, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAdapterResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", errAdapter)
	}
	if len(raw) > maxAdapterResponseBytes {
		return nil, fmt.Errorf("%w: response is larger than %d bytes", errAdapter, maxAdapterResponseBytes)
	}
	return raw, nil
}

// codePointOffsets returns the byte offset of each code point of content,
// plus len(content) at the end. Index i is the byte offset of code point i.
func codePointOffsets(content string) []int {
	out := make([]int, 0, utf8.RuneCountInString(content)+1)
	for i := range content {
		out = append(out, i)
	}
	return append(out, len(content))
}
