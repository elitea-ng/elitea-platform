package guardrail

// adapter_cloud.go holds the parts that the first-party cloud classifier
// adapters share (ADR-0043 decision 1): adapter_azure.go (Azure AI Content
// Safety), adapter_bedrock.go (AWS Bedrock Guardrails) and
// adapter_modelarmor.go (Google Cloud Model Armor).
//
// Each adapter uses the same envelope as the generic http adapter
// (httpadapter.go):
//
//   - The call is EGRESS. The endpoint host must be on the gateway's egress
//     allowlist. Env.HTTPClient never follows a redirect.
//   - The response body is bounded (maxAdapterResponseBytes).
//   - The row timeout bounds the call. A failure is an error, and the row's
//     fail mode applies.
//   - An error message names the failure. It never contains the content, a
//     vendor response body or a secret value.
//   - A finding carries a label (ValidLabel), a score in 0..1 and a byte
//     span, or Whole when the vendor returns no location.
//
// A vendor result that reports a prompt attack or a jailbreak always has the
// label LabelPromptInjection.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// LabelPromptInjection is the label of a cloud adapter finding that reports a
// prompt attack or a jailbreak.
const LabelPromptInjection = "prompt_injection"

// maxLocateMatch bounds the vendor match text that locateAll searches for.
const maxLocateMatch = 4096

// cloudEndpoint parses an endpoint URL of a cloud adapter. It must be an
// absolute https URL with no credentials, query or fragment. The result has
// no trailing slash.
func cloudEndpoint(field, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || raw == "" || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("%s must be an absolute https URL", field)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not carry credentials", field)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must not have a query or a fragment", field)
	}
	return strings.TrimRight(raw, "/"), nil
}

// secretRef reads an optional {{secret.NAME}} field. It refuses a raw value.
func secretRef(obj map[string]any, field string) (string, error) {
	raw, present := obj[field]
	if !present || raw == nil || raw == "" {
		return "", nil
	}
	ref, ok := raw.(string)
	if !ok || !isSecretRef(ref) {
		return "", fmt.Errorf("%s must be a {{secret.NAME}} reference to the secrets vault, never a raw value", field)
	}
	return ref, nil
}

// resolveSecret resolves a {{secret.NAME}} reference. The error names the
// field, never the value.
func resolveSecret(ctx context.Context, env Env, field, ref string) (string, error) {
	if env.ResolveSecret == nil {
		return "", fmt.Errorf("%w: no secrets vault is available for %s", errAdapter, field)
	}
	value, err := env.ResolveSecret(ctx, ref)
	if err != nil || value == "" {
		return "", fmt.Errorf("%w: %s does not resolve", errAdapter, field)
	}
	return value, nil
}

// cloudPost sends one JSON POST to target and returns the body of a 200
// answer. prepare adds the auth headers (and, for SigV4, signs the request).
func cloudPost(ctx context.Context, env Env, target string, body []byte, prepare func(*http.Request) error) ([]byte, error) {
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

// locateAll returns the byte spans of every occurrence of match in content.
// Some vendors return the matched text and no offset. The gateway uses that
// text only to find the spans, and then drops it. fold makes the search
// ignore case. The result is empty when match is empty, too long or absent.
func locateAll(content, match string, fold bool) []bound {
	if match == "" || len(match) > maxLocateMatch {
		return nil
	}
	expr := regexp.QuoteMeta(match)
	if fold {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil
	}
	locs := re.FindAllStringIndex(content, maxAdapterFindings)
	out := make([]bound, 0, len(locs))
	for _, l := range locs {
		out = append(out, bound{start: l[0], end: l[1]})
	}
	return out
}

// spanFindings returns one finding per span, or one Whole finding when there
// is no span.
func spanFindings(spans []bound, label string, score float64) []Finding {
	if len(spans) == 0 {
		return []Finding{{Whole: true, Label: label, Score: score}}
	}
	out := make([]Finding, 0, len(spans))
	for _, s := range spans {
		out = append(out, Finding{Start: s.start, End: s.end, Label: label, Score: score})
	}
	return out
}

// vendorLabel turns a vendor type name (EMAIL, US_SOCIAL_SECURITY_NUMBER,
// "Medical advice") into a label under prefix: lower case, and every
// character outside [a-z0-9_.:-] becomes "_". It returns fallback when the
// result is not a valid label.
func vendorLabel(prefix, name, fallback string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	label := b.String()
	if len(label) > 64 {
		label = label[:64]
	}
	if label == prefix || !ValidLabel(label) {
		return fallback
	}
	return label
}

// codePointChunks splits content into parts of at most limit Unicode code
// points. A part never splits a character.
func codePointChunks(content string, limit int) []bound {
	if content == "" {
		return []bound{{0, 0}}
	}
	var out []bound
	start, count := 0, 0
	for i := range content {
		if count == limit {
			out = append(out, bound{start: start, end: i})
			start, count = i, 0
		}
		count++
	}
	return append(out, bound{start: start, end: len(content)})
}

// codePointSpan converts a half-open range of Unicode code points to byte
// offsets in content. ok is false when the range is outside the content.
func codePointSpan(content string, start, end int64) (bound, bool) {
	if start < 0 || end < start {
		return bound{}, false
	}
	var b bound
	idx := int64(0)
	found := false
	for i := range content {
		if idx == start {
			b.start, found = i, true
		}
		if idx == end {
			b.end = i
			return b, found
		}
		idx++
	}
	if idx == start {
		b.start, found = len(content), true
	}
	if idx == end {
		b.end = len(content)
		return b, found
	}
	return bound{}, false
}

// byteSpan checks a UTF-8 byte range that a vendor returned. It must be
// inside content and on character boundaries.
func byteSpan(content string, start, end int64) (bound, bool) {
	if start < 0 || end < start || end > int64(len(content)) {
		return bound{}, false
	}
	s, e := int(start), int(end)
	if (s < len(content) && !utf8.RuneStart(content[s])) || (e < len(content) && !utf8.RuneStart(content[e])) {
		return bound{}, false
	}
	return bound{start: s, end: e}, true
}
