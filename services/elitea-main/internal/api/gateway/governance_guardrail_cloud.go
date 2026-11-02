package gateway

// governance_guardrail_cloud.go — the write-side check of the first-party
// cloud classifier detectors of a guardrail entry (ADR-0043 decision 1):
// azure_content_safety, bedrock_guardrails and model_armor. The gateway
// compiles them in elitea-llm-gateway internal/guardrail/adapter_*.go. The
// shared case file (testdata/rows.json there) keeps the two checks equal.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	guardrailAzureCategories    = map[string]bool{"Hate": true, "Sexual": true, "Violence": true, "SelfHarm": true}
	guardrailAzureBlocklist     = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,64}$`)
	guardrailAWSRegion          = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]{1,2}$`)
	guardrailBedrockID          = regexp.MustCompile(`^(?:[a-z0-9]{1,128}|arn:aws(?:-[^:]+)?:bedrock:[a-z0-9-]{1,20}:[0-9]{12}:guardrail/[a-z0-9]{1,128})$`)
	guardrailBedrockVersion     = regexp.MustCompile(`^(?:[1-9][0-9]{0,7}|DRAFT)$`)
	guardrailModelArmorTmpl     = regexp.MustCompile(`^projects/[a-z0-9][a-z0-9-]{0,62}/locations/[a-z][a-z0-9-]{0,62}/templates/[A-Za-z0-9_-]{1,63}$`)
	guardrailAzureMaxBlocklists = 10
)

// validateGuardrailCloudDetector checks a cloud detector. handled is false
// when typ is not a cloud detector type.
func validateGuardrailCloudDetector(typ string, obj map[string]any) (handled bool, err error) {
	switch typ {
	case "azure_content_safety":
		return true, validateGuardrailAzure(obj)
	case "bedrock_guardrails":
		return true, validateGuardrailBedrock(obj)
	case "model_armor":
		return true, validateGuardrailModelArmor(obj)
	}
	return false, nil
}

func guardrailHTTPSEndpoint(field string, v any) error {
	raw, _ := v.(string)
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || raw == "" || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute https URL", field)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not carry credentials", field)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not have a query or a fragment", field)
	}
	return nil
}

// guardrailSecretField reads an optional {{secret.NAME}} field.
func guardrailSecretField(obj map[string]any, field string) (bool, error) {
	raw, present := obj[field]
	if !present || raw == nil || raw == "" {
		return false, nil
	}
	ref, ok := raw.(string)
	if !ok || !guardrailSecretPattern.MatchString(ref) {
		return false, fmt.Errorf("%s must be a {{secret.NAME}} reference to the secrets vault, never a raw value", field)
	}
	return true, nil
}

func validateGuardrailAzure(obj map[string]any) error {
	if err := guardrailHTTPSEndpoint("endpoint", obj["endpoint"]); err != nil {
		return err
	}
	hasKey, err := guardrailSecretField(obj, "api_key_secret")
	if err != nil {
		return err
	}
	if !hasKey {
		return errors.New("api_key_secret is required: a {{secret.NAME}} reference to the Content Safety resource key")
	}
	maxSeverity := int64(6)
	if v, present := obj["output_type"]; present && v != nil && v != "" {
		switch s, _ := v.(string); s {
		case "FourSeverityLevels":
		case "EightSeverityLevels":
			maxSeverity = 7
		default:
			return errors.New(`output_type must be "FourSeverityLevels" or "EightSeverityLevels"`)
		}
	}
	categories := 0
	if v, present := obj["categories"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("categories must be an object of category to severity threshold")
		}
		for name, t := range m {
			if !guardrailAzureCategories[name] {
				return fmt.Errorf("categories names %q; use Hate, Sexual, Violence or SelfHarm", name)
			}
			n, ok := guardrailWhole(t)
			if !ok || n < 1 || n > maxSeverity {
				return fmt.Errorf("categories.%s must be a whole number between 1 and %d", name, maxSeverity)
			}
		}
		categories = len(m)
	}
	blocklists, err := guardrailStrings(obj, "blocklists")
	if err != nil {
		return err
	}
	if len(blocklists) > guardrailAzureMaxBlocklists {
		return fmt.Errorf("blocklists lists %d entries; the limit is %d", len(blocklists), guardrailAzureMaxBlocklists)
	}
	for _, b := range blocklists {
		if !guardrailAzureBlocklist.MatchString(b) {
			return fmt.Errorf("blocklist name %q must match %s", b, guardrailAzureBlocklist.String())
		}
	}
	shields := false
	if v, present := obj["prompt_shields"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errors.New("prompt_shields must be a boolean")
		}
		shields = b
	}
	if categories == 0 && len(blocklists) == 0 && !shields {
		return errors.New("an azure_content_safety detector needs categories, blocklists or prompt_shields")
	}
	return nil
}

func validateGuardrailBedrock(obj map[string]any) error {
	if s, _ := obj["region"].(string); !guardrailAWSRegion.MatchString(s) {
		return errors.New("region must be an AWS region name, for example us-east-1")
	}
	if s, _ := obj["guardrail_identifier"].(string); !guardrailBedrockID.MatchString(s) {
		return errors.New("guardrail_identifier must be a guardrail id or a guardrail ARN")
	}
	if s, _ := obj["guardrail_version"].(string); !guardrailBedrockVersion.MatchString(s) {
		return errors.New(`guardrail_version must be a version number or "DRAFT"`)
	}
	if raw, ok := obj["endpoint"].(string); ok && raw != "" {
		if err := guardrailHTTPSEndpoint("endpoint", raw); err != nil {
			return err
		}
	}
	hasID, err := guardrailSecretField(obj, "access_key_id_secret")
	if err != nil {
		return err
	}
	hasSecret, err := guardrailSecretField(obj, "secret_access_key_secret")
	if err != nil {
		return err
	}
	if _, err := guardrailSecretField(obj, "session_token_secret"); err != nil {
		return err
	}
	if !hasID || !hasSecret {
		return errors.New("access_key_id_secret and secret_access_key_secret are required {{secret.NAME}} references")
	}
	return nil
}

func validateGuardrailModelArmor(obj map[string]any) error {
	if s, _ := obj["template"].(string); !guardrailModelArmorTmpl.MatchString(s) {
		return errors.New("template must be projects/PROJECT/locations/LOCATION/templates/TEMPLATE")
	}
	if raw, ok := obj["endpoint"].(string); ok && raw != "" {
		if err := guardrailHTTPSEndpoint("endpoint", raw); err != nil {
			return err
		}
	}
	hasSecret, err := guardrailSecretField(obj, "credentials_secret")
	if err != nil {
		return err
	}
	switch mode, _ := obj["auth_mode"].(string); mode {
	case "", "key":
		if !hasSecret {
			return errors.New(`credentials_secret is required when auth_mode is "key"`)
		}
	case "workload_identity":
		if hasSecret {
			return errors.New(`credentials_secret must be empty when auth_mode is "workload_identity"`)
		}
	default:
		return errors.New(`auth_mode must be "key" or "workload_identity"`)
	}
	return nil
}
