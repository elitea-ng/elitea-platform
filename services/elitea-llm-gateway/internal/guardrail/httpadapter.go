package guardrail

// The generic HTTP adapter (ADR-0043 decision 1). It sends the content to a
// classifier the customer runs and reads findings back, with a small fixed
// contract. The JSON schemas of the contract are in
// libs/jsonschema/guardrail/v1/ and httpadapter_contract_test.go keeps the
// structs below equal to them.
//
// The call is EGRESS, not internal traffic: it leaves the platform for a
// customer endpoint. It passes the gateway's egress allowlist (the one that
// gates provider api_base hosts), never follows a redirect, and reaches a
// private address only when the allowlist names a private destination.

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

// ContractVersion is the value of the request's `version` field.
const ContractVersion = "elitea.guardrail.http.v1"

// maxAdapterResponseBytes bounds the response body the gateway reads.
const maxAdapterResponseBytes = 1 << 20

// maxAdapterFindings bounds the findings one response may carry.
const maxAdapterFindings = 1000

// AdapterRequest is the body the gateway POSTs to the adapter URL.
type AdapterRequest struct {
	Version  string `json:"version"`
	Stage    string `json:"stage"`
	Detector string `json:"detector"`
	Content  string `json:"content"`
}

// AdapterResponse is the body the adapter returns with status 200.
type AdapterResponse struct {
	Findings []AdapterFinding `json:"findings"`
}

// AdapterFinding is one classifier result. Start and End are offsets in
// Unicode code points (characters) of the request content, End exclusive.
// When both are absent the finding covers the whole content.
type AdapterFinding struct {
	Label string   `json:"label"`
	Score *float64 `json:"score,omitempty"`
	Start *int     `json:"start,omitempty"`
	End   *int     `json:"end,omitempty"`
}

// HTTPConfig is the authored configuration of an http detector.
type HTTPConfig struct {
	URL string
	// AuthHeader is the request header that carries the secret, default
	// Authorization.
	AuthHeader string
	// AuthSecret is a {{secret.NAME}} reference, never a raw value.
	AuthSecret string
	// Labels, when set, keeps only findings with one of these labels.
	Labels []string
	// MinScore drops findings with a lower score. A finding with no score
	// counts as score 1.
	MinScore float64
}

type httpDetector struct {
	name string
	cfg  HTTPConfig
}

// isSecretRef reports whether s is a {{secret.NAME}} reference, the only
// accepted auth_secret shape.
func isSecretRef(s string) bool {
	const prefix, suffix = "{{secret.", "}}"
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(s, prefix), suffix)
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r != '_' && r != '-' && r != '.' && !isAlnum(r) {
			return false
		}
	}
	return true
}

func newHTTPDetector(obj map[string]any) (detectorImpl, error) {
	cfg := HTTPConfig{AuthHeader: "Authorization"}
	cfg.URL, _ = obj["url"].(string)
	u, err := url.Parse(cfg.URL)
	if err != nil || cfg.URL == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("url must be an absolute http or https URL")
	}
	if u.User != nil {
		return nil, errors.New("url must not carry credentials; use auth_secret")
	}
	if h, ok := obj["auth_header"].(string); ok && h != "" {
		if !validHeaderName(h) {
			return nil, fmt.Errorf("auth_header %q is not a valid header name", h)
		}
		cfg.AuthHeader = h
	}
	if s, present := obj["auth_secret"]; present && s != nil {
		ref, ok := s.(string)
		if !ok || !isSecretRef(ref) {
			return nil, errors.New("auth_secret must be a {{secret.NAME}} reference to the secrets vault, never a raw value")
		}
		cfg.AuthSecret = ref
	}
	labels, err := stringList(obj, "labels")
	if err != nil {
		return nil, err
	}
	if len(labels) > MaxLabels {
		return nil, fmt.Errorf("labels lists %d entries; the limit is %d", len(labels), MaxLabels)
	}
	for _, l := range labels {
		if !ValidLabel(l) {
			return nil, fmt.Errorf("label %q must match %s", l, labelPattern.String())
		}
	}
	cfg.Labels = labels
	if v, present := obj["min_score"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 0 || f > 1 {
			return nil, errors.New("min_score must be a number between 0 and 1")
		}
		cfg.MinScore = f
	}
	return &httpDetector{cfg: cfg}, nil
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func validHeaderName(h string) bool {
	if len(h) > 64 {
		return false
	}
	for _, r := range h {
		if r != '-' && !isAlnum(r) {
			return false
		}
	}
	return true
}

// errAdapter is the class of every adapter failure. The message names the
// failure and never the content or the secret.
var errAdapter = errors.New("guardrail adapter")

func (d *httpDetector) detect(ctx context.Context, env Env, stage, content string) ([]Finding, error) {
	if env.HTTPClient == nil || env.EgressAllows == nil || !env.EgressAllows(d.cfg.URL) {
		return nil, fmt.Errorf("%w: the adapter host is not on the egress allowlist", errAdapter)
	}
	body, err := json.Marshal(AdapterRequest{Version: ContractVersion, Stage: stage, Detector: d.name, Content: content})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", errAdapter, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", errAdapter)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.cfg.AuthSecret != "" {
		if env.ResolveSecret == nil {
			return nil, fmt.Errorf("%w: no secrets vault is available for auth_secret", errAdapter)
		}
		value, rerr := env.ResolveSecret(ctx, d.cfg.AuthSecret)
		if rerr != nil || value == "" {
			return nil, fmt.Errorf("%w: auth_secret does not resolve", errAdapter)
		}
		req.Header.Set(d.cfg.AuthHeader, value)
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
	var parsed AdapterResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: response is not the v1 contract", errAdapter)
	}
	if len(parsed.Findings) > maxAdapterFindings {
		return nil, fmt.Errorf("%w: response has more than %d findings", errAdapter, maxAdapterFindings)
	}
	return d.findings(parsed.Findings, content)
}

// findings converts adapter findings to byte-span findings, applying the
// label and score filters.
func (d *httpDetector) findings(in []AdapterFinding, content string) ([]Finding, error) {
	var offsets []int // code point index -> byte offset, built on demand
	runeCount := -1
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		if !ValidLabel(f.Label) {
			return nil, fmt.Errorf("%w: a finding label is not valid", errAdapter)
		}
		if len(d.cfg.Labels) > 0 && !containsString(d.cfg.Labels, f.Label) {
			continue
		}
		score := 1.0
		if f.Score != nil {
			score = *f.Score
			if score < 0 || score > 1 {
				return nil, fmt.Errorf("%w: a finding score is outside 0..1", errAdapter)
			}
		}
		if score < d.cfg.MinScore {
			continue
		}
		if f.Start == nil && f.End == nil {
			out = append(out, Finding{Whole: true, Label: f.Label, Score: score})
			continue
		}
		if f.Start == nil || f.End == nil {
			return nil, fmt.Errorf("%w: a finding has start without end", errAdapter)
		}
		if offsets == nil {
			offsets = make([]int, 0, utf8.RuneCountInString(content)+1)
			for i := range content {
				offsets = append(offsets, i)
			}
			offsets = append(offsets, len(content))
			runeCount = len(offsets) - 1
		}
		s, e := *f.Start, *f.End
		if s < 0 || e < s || e > runeCount {
			return nil, fmt.Errorf("%w: a finding span is outside the content", errAdapter)
		}
		if s == e {
			continue
		}
		out = append(out, Finding{Start: offsets[s], End: offsets[e], Label: f.Label, Score: score})
	}
	return out, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
