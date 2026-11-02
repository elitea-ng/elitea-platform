package guardrail

// adapter_post.go holds the one egress call that every network adapter uses:
// the generic http adapter (httpadapter.go), the cloud adapters
// (adapter_cloud.go) and the self-hosted adapters (adapter_selfhosted.go).
//
//   - The call is EGRESS. The target must be on the gateway's egress
//     allowlist. Env.HTTPClient never follows a redirect.
//   - The request body is JSON. A status other than 200 is an error.
//   - The response body is bounded (maxAdapterResponseBytes).
//   - An error message names the failure. It never contains the content, a
//     vendor response body or a secret value.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// egressPost sends one JSON POST to target and returns the body of a 200
// answer. prepare adds the auth headers (and, for SigV4, signs the request).
// A nil prepare adds nothing.
func egressPost(ctx context.Context, env Env, target string, body []byte, prepare func(*http.Request) error) ([]byte, error) {
	if env.HTTPClient == nil || env.EgressAllows == nil || !env.EgressAllows(target) {
		return nil, fmt.Errorf("%w: the adapter host is not on the egress allowlist", errAdapter)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", errAdapter)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if prepare != nil {
		if err := prepare(req); err != nil {
			return nil, err
		}
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
		// The vendor error body is not read into the message: it can quote
		// the request.
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

// secretHeader returns a prepare function for egressPost. It resolves the
// {{secret.NAME}} reference ref and sets header to the value. prefix (for
// example "Bearer ") goes before the value when the value does not already
// start with it. An empty ref adds no header. field names the reference in
// an error.
func secretHeader(ctx context.Context, env Env, field, ref, header, prefix string) func(*http.Request) error {
	return func(r *http.Request) error {
		if ref == "" {
			return nil
		}
		value, err := resolveSecret(ctx, env, field, ref)
		if err != nil {
			return err
		}
		if prefix != "" && !strings.HasPrefix(value, prefix) {
			value = prefix + value
		}
		r.Header.Set(header, value)
		return nil
	}
}

// codePointOffsets returns the byte offset of each code point of content,
// plus len(content) at the end. Index i is the byte offset of code point i.
// Adapters use it to convert code point spans to byte spans.
func codePointOffsets(content string) []int {
	out := make([]int, 0, utf8.RuneCountInString(content)+1)
	for i := range content {
		out = append(out, i)
	}
	return append(out, len(content))
}
