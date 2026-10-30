package gateway

// governance_guardrail.go — the `guardrail` entry (ADR-0043 decision 1,
// issue #1500).
//
// A guardrail entry is a content control the LLM gateway runs on the /llm
// path: a stage (input or output), an ordered list of detectors with an
// action each (block, mask, flag), a timeout and a fail mode. The gateway
// compiles the entry on load (elitea-llm-gateway internal/guardrail ParseRow)
// and rejects one it cannot use. This file refuses the same entries on write,
// so the operator sees the reason at once.
//
// The gateway is a separate Go module that elitea-main does not import, so
// the two checks are separate source. governance_guardrail_test.go runs this
// check over the gateway's own case file
// (elitea-llm-gateway/internal/guardrail/testdata/rows.json), and the gateway
// runs ParseRow over the same file: a case the two disagree on fails one of
// the two suites.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// guardrailType is the governance entry type of a guardrail.
const guardrailType = "guardrail"

// The bounds the gateway applies (internal/guardrail/spec.go).
const (
	guardrailMaxDetectors     = 16
	guardrailMaxPatterns      = 64
	guardrailMaxPatternBytes  = 1024
	guardrailMaxTerms         = 1000
	guardrailMaxTermBytes     = 256
	guardrailMaxTermsTotal    = 64 << 10
	guardrailMaxLabels        = 32
	guardrailMinTimeoutMS     = 50
	guardrailMaxTimeoutMS     = 30000
	guardrailMinStreamWindow  = 64
	guardrailMaxStreamWindow  = 8192
	guardrailMaxSecretNameLen = 128
)

var (
	guardrailLabelPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,63}$`)
	guardrailHeaderPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	guardrailSecretPattern = regexp.MustCompile(`^\{\{secret\.[A-Za-z0-9_.-]{1,128}\}\}$`)
)

// guardrailSecretRuleIDs are the rule ids of the gateway's secrets detector
// (libs/rust/local-tools/src/handoff/secret_rules.json). The test pins the
// list to that file.
var guardrailSecretRuleIDs = []string{
	"aws-access-key-id", "aws-secret-access-key", "private-key", "github-token",
	"github-fine-grained-token", "gitlab-token", "slack-token", "slack-webhook",
	"connection-string-password", "password-assignment", "jwt",
}

// validateGuardrailData refuses a guardrail entry the gateway would reject.
func validateGuardrailData(data map[string]any) error {
	g, _ := data["guardrail"].(map[string]any)
	if len(g) == 0 {
		g = data
	}
	stage, _ := g["stage"].(string)
	if stage != "input" && stage != "output" {
		return errors.New(`guardrail.stage must be "input" or "output"`)
	}
	raw, ok := g["detectors"].([]any)
	if !ok || len(raw) == 0 {
		return errors.New("guardrail.detectors must list at least one detector")
	}
	if len(raw) > guardrailMaxDetectors {
		return fmt.Errorf("guardrail.detectors lists %d detectors; the limit is %d", len(raw), guardrailMaxDetectors)
	}
	names := map[string]bool{}
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("guardrail.detectors[%d] must be an object", i)
		}
		name, err := validateGuardrailDetector(obj)
		if err != nil {
			return fmt.Errorf("guardrail.detectors[%d]: %w", i, err)
		}
		if names[name] {
			return fmt.Errorf("guardrail.detectors[%d]: the name %q is used twice", i, name)
		}
		names[name] = true
	}
	if v, present := g["timeout_ms"]; present && v != nil {
		ms, ok := guardrailWhole(v)
		if !ok {
			return errors.New("guardrail.timeout_ms must be a whole number")
		}
		if ms < guardrailMinTimeoutMS || ms > guardrailMaxTimeoutMS {
			return fmt.Errorf("guardrail.timeout_ms must be between %d and %d", guardrailMinTimeoutMS, guardrailMaxTimeoutMS)
		}
	}
	switch mode, _ := g["fail_mode"].(string); mode {
	case "", "open", "closed":
	default:
		return errors.New(`guardrail.fail_mode must be "open" or "closed"`)
	}
	if v, present := g["fail_mode"]; present && v != nil {
		if _, ok := v.(string); !ok {
			return errors.New(`guardrail.fail_mode must be "open" or "closed"`)
		}
	}
	if v, present := g["apply_to_embeddings"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errors.New("guardrail.apply_to_embeddings must be a boolean")
		}
		if b && stage != "input" {
			return errors.New("guardrail.apply_to_embeddings applies to input rows only: embeddings have no text output")
		}
	}
	if v, present := g["stream_window_bytes"]; present && v != nil {
		n, ok := guardrailWhole(v)
		if !ok || n < guardrailMinStreamWindow || n > guardrailMaxStreamWindow {
			return fmt.Errorf("guardrail.stream_window_bytes must be a whole number between %d and %d",
				guardrailMinStreamWindow, guardrailMaxStreamWindow)
		}
	}
	return nil
}

func validateGuardrailDetector(obj map[string]any) (string, error) {
	typ, _ := obj["type"].(string)
	action, _ := obj["action"].(string)
	name, _ := obj["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		name = typ
	}
	if !guardrailLabelPattern.MatchString(name) {
		return "", fmt.Errorf("name %q must match %s", name, guardrailLabelPattern.String())
	}
	switch action {
	case "block", "mask", "flag":
	default:
		return "", errors.New(`action must be "block", "mask" or "flag"`)
	}
	switch typ {
	case "regex":
		return name, validateGuardrailRegex(obj)
	case "secrets":
		rules, err := guardrailStrings(obj, "rules")
		if err != nil {
			return "", err
		}
		for _, id := range rules {
			if !containsString(guardrailSecretRuleIDs, id) {
				return "", fmt.Errorf("rules names %q, which is not a secret rule id", id)
			}
		}
		return name, nil
	case "http":
		return name, validateGuardrailHTTP(obj)
	}
	return "", errors.New(`type must be "regex", "secrets" or "http"`)
}

func validateGuardrailRegex(obj map[string]any) error {
	if label, present := obj["label"]; present && label != nil && label != "" {
		s, ok := label.(string)
		if !ok || !guardrailLabelPattern.MatchString(s) {
			return fmt.Errorf("label %v must match %s", label, guardrailLabelPattern.String())
		}
	}
	patterns, err := guardrailStrings(obj, "patterns")
	if err != nil {
		return err
	}
	terms, err := guardrailStrings(obj, "terms")
	if err != nil {
		return err
	}
	if len(patterns) == 0 && len(terms) == 0 {
		return errors.New("a regex detector needs at least one pattern or term")
	}
	if len(patterns) > guardrailMaxPatterns {
		return fmt.Errorf("patterns lists %d entries; the limit is %d", len(patterns), guardrailMaxPatterns)
	}
	for i, p := range patterns {
		if len(p) > guardrailMaxPatternBytes {
			return fmt.Errorf("patterns[%d] is %d bytes; the limit is %d", i, len(p), guardrailMaxPatternBytes)
		}
		// Go's regexp is RE2, the engine the gateway compiles with.
		re, cerr := regexp.Compile(p)
		if cerr != nil {
			return fmt.Errorf("patterns[%d] is not a valid RE2 expression: %w", i, cerr)
		}
		if re.MatchString("") {
			return fmt.Errorf("patterns[%d] matches the empty string, so it would match everywhere", i)
		}
	}
	if len(terms) > guardrailMaxTerms {
		return fmt.Errorf("terms lists %d entries; the limit is %d", len(terms), guardrailMaxTerms)
	}
	total := 0
	for i, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			return fmt.Errorf("terms[%d] is blank", i)
		}
		if len(t) > guardrailMaxTermBytes {
			return fmt.Errorf("terms[%d] is %d bytes; the limit is %d", i, len(t), guardrailMaxTermBytes)
		}
		total += len(t)
	}
	if total > guardrailMaxTermsTotal {
		return fmt.Errorf("terms total %d bytes; the limit is %d", total, guardrailMaxTermsTotal)
	}
	return nil
}

func validateGuardrailHTTP(obj map[string]any) error {
	raw, _ := obj["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || raw == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("url must be an absolute http or https URL")
	}
	if u.User != nil {
		return errors.New("url must not carry credentials; use auth_secret")
	}
	if h, ok := obj["auth_header"].(string); ok && h != "" && !guardrailHeaderPattern.MatchString(h) {
		return fmt.Errorf("auth_header %q is not a valid header name", h)
	}
	if s, present := obj["auth_secret"]; present && s != nil {
		ref, ok := s.(string)
		if !ok || !guardrailSecretPattern.MatchString(ref) {
			return errors.New("auth_secret must be a {{secret.NAME}} reference to the secrets vault, never a raw value")
		}
	}
	labels, err := guardrailStrings(obj, "labels")
	if err != nil {
		return err
	}
	if len(labels) > guardrailMaxLabels {
		return fmt.Errorf("labels lists %d entries; the limit is %d", len(labels), guardrailMaxLabels)
	}
	for _, l := range labels {
		if !guardrailLabelPattern.MatchString(l) {
			return fmt.Errorf("label %q must match %s", l, guardrailLabelPattern.String())
		}
	}
	if v, present := obj["min_score"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 0 || f > 1 {
			return errors.New("min_score must be a number between 0 and 1")
		}
	}
	return nil
}

func guardrailStrings(obj map[string]any, key string) ([]string, error) {
	raw, present := obj[key]
	if !present || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings", key)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("every %s entry must be a string", key)
		}
		if s == "" {
			return nil, fmt.Errorf("a %s entry must not be empty", key)
		}
		out = append(out, s)
	}
	return out, nil
}

func guardrailWhole(v any) (int64, bool) {
	f, ok := toFloat(v)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
