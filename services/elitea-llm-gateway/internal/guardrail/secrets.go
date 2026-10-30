package guardrail

// The secrets detector reuses the platform's credential rule set: the
// gitleaks-style rules that keep credentials out of a desktop handoff and out
// of the worker's logs (ADR-0034 decision 5). The rules ship as data in
// libs/rust/local-tools/src/handoff/secret_rules.json. Go cannot embed a file
// from outside its module, so secret_rules.json here is a byte copy of that
// file, and secrets_pin_test.go fails when the two differ. Edit the Rust file
// and copy it here; do not edit this copy alone.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
)

//go:embed secret_rules.json
var secretRulesJSON []byte

type secretRule struct {
	id          string
	re          *regexp.Regexp
	secretGroup int
	allow       []*regexp.Regexp
}

var (
	secretRulesOnce sync.Once
	secretRules     []secretRule
	secretRulesErr  error
)

// loadSecretRules compiles the embedded rule set once. Every rule is RE2.
func loadSecretRules() ([]secretRule, error) {
	secretRulesOnce.Do(func() {
		var doc struct {
			Rules []struct {
				ID          string   `json:"id"`
				Regex       string   `json:"regex"`
				SecretGroup int      `json:"secret_group"`
				Allow       []string `json:"allow"`
			} `json:"rules"`
		}
		if err := json.Unmarshal(secretRulesJSON, &doc); err != nil {
			secretRulesErr = fmt.Errorf("secret rules: %w", err)
			return
		}
		for _, r := range doc.Rules {
			re, err := regexp.Compile(r.Regex)
			if err != nil {
				secretRulesErr = fmt.Errorf("secret rule %s: %w", r.ID, err)
				return
			}
			if r.SecretGroup < 0 || r.SecretGroup > re.NumSubexp() {
				secretRulesErr = fmt.Errorf("secret rule %s: secret_group %d is out of range", r.ID, r.SecretGroup)
				return
			}
			rule := secretRule{id: r.ID, re: re, secretGroup: r.SecretGroup}
			for _, a := range r.Allow {
				are, err := regexp.Compile(a)
				if err != nil {
					secretRulesErr = fmt.Errorf("secret rule %s allow: %w", r.ID, err)
					return
				}
				rule.allow = append(rule.allow, are)
			}
			secretRules = append(secretRules, rule)
		}
	})
	return secretRules, secretRulesErr
}

// SecretRuleIDs lists the rule ids, which are the labels the secrets detector
// reports.
func SecretRuleIDs() []string {
	rules, _ := loadSecretRules()
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.id)
	}
	return out
}

type secretsDetector struct {
	rules []secretRule
}

// newSecretsDetector selects the whole rule set, or the ids listed in `rules`.
func newSecretsDetector(obj map[string]any) (detectorImpl, error) {
	all, err := loadSecretRules()
	if err != nil {
		return nil, err
	}
	want, err := stringList(obj, "rules")
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return &secretsDetector{rules: all}, nil
	}
	byID := map[string]secretRule{}
	for _, r := range all {
		byID[r.id] = r
	}
	d := &secretsDetector{}
	for _, id := range want {
		r, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("rules names %q, which is not a secret rule id", id)
		}
		d.rules = append(d.rules, r)
	}
	return d, nil
}

func (d *secretsDetector) detect(_ context.Context, _ Env, _ string, content string) ([]Finding, error) {
	var out []Finding
	for _, r := range d.rules {
		for _, m := range r.re.FindAllStringSubmatchIndex(content, -1) {
			start, end := m[2*r.secretGroup], m[2*r.secretGroup+1]
			if start < 0 {
				continue
			}
			if allowed(r.allow, content[start:end]) {
				continue
			}
			out = append(out, Finding{Start: start, End: end, Label: r.id, Score: 1})
		}
	}
	return out, nil
}

func allowed(allow []*regexp.Regexp, secret string) bool {
	for _, a := range allow {
		if a.MatchString(secret) {
			return true
		}
	}
	return false
}
