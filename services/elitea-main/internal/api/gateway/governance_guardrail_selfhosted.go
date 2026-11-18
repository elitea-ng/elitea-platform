package gateway

// governance_guardrail_selfhosted.go — the write-side check of the
// self-hosted classifier detectors (llama_guard, presidio) and of the
// content scope and the prompt_injection slot (ADR-0043 decisions 1 and 4).
// The gateway's checks are internal/guardrail/adapter_llamaguard.go,
// adapter_presidio.go and slot.go. The shared case file keeps the two equal.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	guardrailMaxModelBytes       = 256
	guardrailLlamaGuardMaxCodes  = 14
	guardrailMaxPresidioEntities = 64
	guardrailSlotPromptInjection = "prompt_injection"
	guardrailContentToolResults  = "tool_results"
)

var (
	guardrailLlamaGuardCode  = regexp.MustCompile(`^S[0-9]{1,2}$`)
	guardrailPresidioEntity  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	guardrailPresidioLangTag = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)
)

// validateGuardrailEndpoint checks url and auth_secret of a self-hosted
// adapter.
func validateGuardrailEndpoint(obj map[string]any) error {
	raw, _ := obj["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || raw == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("url must be an absolute http or https URL")
	}
	if u.User != nil {
		return errors.New("url must not carry credentials; use auth_secret")
	}
	if s, present := obj["auth_secret"]; present && s != nil {
		ref, ok := s.(string)
		if !ok || !guardrailSecretPattern.MatchString(ref) {
			return errors.New("auth_secret must be a {{secret.NAME}} reference to the secrets vault, never a raw value")
		}
	}
	return nil
}

func validateGuardrailLlamaGuard(obj map[string]any) error {
	if err := validateGuardrailEndpoint(obj); err != nil {
		return err
	}
	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" || len(model) > guardrailMaxModelBytes {
		return fmt.Errorf("model must name the served Llama Guard model (1 to %d bytes)", guardrailMaxModelBytes)
	}
	cats, err := guardrailStrings(obj, "categories")
	if err != nil {
		return err
	}
	if len(cats) > guardrailLlamaGuardMaxCodes {
		return fmt.Errorf("categories lists %d entries; the limit is %d", len(cats), guardrailLlamaGuardMaxCodes)
	}
	for _, c := range cats {
		if !guardrailLlamaGuardCode.MatchString(c) {
			return fmt.Errorf("categories entry %q must be a category code such as S1", c)
		}
	}
	return nil
}

func validateGuardrailPresidio(obj map[string]any) error {
	if err := validateGuardrailEndpoint(obj); err != nil {
		return err
	}
	if v, present := obj["language"]; present && v != nil {
		s, ok := v.(string)
		if !ok || !guardrailPresidioLangTag.MatchString(s) {
			return errors.New("language must be a language code such as en or de")
		}
	}
	entities, err := guardrailStrings(obj, "entities")
	if err != nil {
		return err
	}
	if len(entities) > guardrailMaxPresidioEntities {
		return fmt.Errorf("entities lists %d entries; the limit is %d", len(entities), guardrailMaxPresidioEntities)
	}
	for _, e := range entities {
		if !guardrailPresidioEntity.MatchString(e) {
			return fmt.Errorf("entities entry %q must be a Presidio entity type such as EMAIL_ADDRESS", e)
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

// validateGuardrailScope checks the content and slot options of a detector
// of a row with the given stage.
func validateGuardrailScope(stage, typ, action string, obj map[string]any) error {
	content := "all"
	if v, present := obj["content"]; present && v != nil {
		s, _ := v.(string)
		if s != "all" && s != guardrailContentToolResults {
			return fmt.Errorf(`content must be "all" or %q`, guardrailContentToolResults)
		}
		content = s
	}
	if content == guardrailContentToolResults && stage != "input" {
		return fmt.Errorf("content %q applies to input rows only: an answer has no tool results", guardrailContentToolResults)
	}
	v, present := obj["slot"]
	if !present || v == nil || v == "" {
		return nil
	}
	if s, _ := v.(string); s != guardrailSlotPromptInjection {
		return fmt.Errorf("slot must be %q", guardrailSlotPromptInjection)
	}
	if content != guardrailContentToolResults {
		return fmt.Errorf("slot %q needs content %q: the slot reads tool results only", guardrailSlotPromptInjection, guardrailContentToolResults)
	}
	if action == "mask" {
		return fmt.Errorf("slot %q allows the actions block and flag; mask cannot remove an injection", guardrailSlotPromptInjection)
	}
	if typ == "secrets" || typ == "presidio" || typ == "personal_data" {
		return fmt.Errorf("a %s detector cannot fill slot %q: it finds data, not instructions", typ, guardrailSlotPromptInjection)
	}
	return nil
}
