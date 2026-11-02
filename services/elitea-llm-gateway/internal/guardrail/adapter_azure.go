package guardrail

// The Azure AI Content Safety adapter (ADR-0043 decision 1). adapter_cloud.go
// describes the envelope that all cloud adapters share.
//
// API: https://learn.microsoft.com/rest/api/contentsafety/ (version
// 2024-09-01).
//
//   - POST {endpoint}/contentsafety/text:analyze runs the harm categories
//     (Hate, Sexual, Violence, SelfHarm) and the named blocklists.
//   - POST {endpoint}/contentsafety/text:shieldPrompt runs Prompt Shields.
//     User content on the input stage is the userPrompt. A unit of tool
//     results (input stage) and a model answer (output stage) are one entry
//     of documents: document attack detection, the check for indirect
//     attacks.
//
// Auth is the resource key in the Ocp-Apim-Subscription-Key header, from a
// {{secret.NAME}} reference. Keyless Microsoft Entra auth is not supported:
// the gateway has no Azure identity of its own, and it refuses ambient
// identity (internal/workloadidentity).
//
// Both operations accept at most 10 000 characters per text. The adapter is
// a boundedReader: the pipeline sends it units of at most that size
// (units.go), and the unit calls run in parallel. The two operations of one
// unit also run in parallel. All calls share the row deadline.
//
// Neither operation returns an offset (the 2024-09-01 responses carry
// severities, blocklist ids and booleans only). So no UTF-16 offset needs a
// conversion, and every finding covers its whole unit.
//
// Score: severity / the highest severity of the output type (6 for
// FourSeverityLevels, 7 for EightSeverityLevels). A blocklist match and a
// Prompt Shields attack have score 1.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"sync"
)

// TypeAzureContentSafety is the detector type of the Azure AI Content Safety
// adapter.
const TypeAzureContentSafety = "azure_content_safety"

const (
	azureAPIVersion    = "2024-09-01"
	azureMaxCodePoints = 10000
	azureMaxBlocklists = 10
	azureKeyHeader     = "Ocp-Apim-Subscription-Key"

	azureOutputFour  = "FourSeverityLevels"
	azureOutputEight = "EightSeverityLevels"

	// LabelBlocklist is the label of an Azure blocklist match.
	LabelBlocklist = "blocklist"
)

// azureCategoryLabels maps the Azure harm categories to labels.
var azureCategoryLabels = map[string]string{
	"Hate":     "hate",
	"Sexual":   "sexual",
	"Violence": "violence",
	"SelfHarm": "self_harm",
}

// azureBlocklistName bounds a blocklist name (Azure: at most 64 characters,
// letters, digits and "-", "_", ".", "~").
var azureBlocklistName = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,64}$`)

// AzureConfig is the authored configuration of an azure_content_safety
// detector.
type AzureConfig struct {
	// Endpoint is the resource URL, e.g. https://NAME.cognitiveservices.azure.com.
	Endpoint string
	// APIKeySecret is a {{secret.NAME}} reference to the resource key.
	APIKeySecret string
	// Thresholds maps an Azure category (Hate, Sexual, Violence, SelfHarm) to
	// the lowest severity that is a finding.
	Thresholds map[string]int
	// OutputType is FourSeverityLevels (default) or EightSeverityLevels.
	OutputType   string
	Blocklists   []string
	PromptShield bool
}

type azureDetector struct {
	cfg AzureConfig
	// categories is the sorted key list of Thresholds, sent in the request.
	categories []string
}

func newAzureDetector(obj map[string]any) (detectorImpl, error) {
	cfg := AzureConfig{OutputType: azureOutputFour, Thresholds: map[string]int{}}
	raw, _ := obj["endpoint"].(string)
	endpoint, err := cloudEndpoint("endpoint", raw)
	if err != nil {
		return nil, err
	}
	cfg.Endpoint = endpoint
	cfg.APIKeySecret, err = secretRef(obj, "api_key_secret")
	if err != nil {
		return nil, err
	}
	if cfg.APIKeySecret == "" {
		return nil, errors.New("api_key_secret is required: a {{secret.NAME}} reference to the Content Safety resource key")
	}
	if v, present := obj["output_type"]; present && v != nil && v != "" {
		s, _ := v.(string)
		if s != azureOutputFour && s != azureOutputEight {
			return nil, fmt.Errorf("output_type must be %q or %q", azureOutputFour, azureOutputEight)
		}
		cfg.OutputType = s
	}
	maxSeverity := cfg.maxSeverity()
	if v, present := obj["categories"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("categories must be an object of category to severity threshold")
		}
		for name, t := range m {
			if _, known := azureCategoryLabels[name]; !known {
				return nil, fmt.Errorf("categories names %q; use Hate, Sexual, Violence or SelfHarm", name)
			}
			n, ok := wholeNumber(t)
			if !ok || n < 1 || n > int64(maxSeverity) {
				return nil, fmt.Errorf("categories.%s must be a whole number between 1 and %d", name, maxSeverity)
			}
			cfg.Thresholds[name] = int(n)
		}
	}
	cfg.Blocklists, err = stringList(obj, "blocklists")
	if err != nil {
		return nil, err
	}
	if len(cfg.Blocklists) > azureMaxBlocklists {
		return nil, fmt.Errorf("blocklists lists %d entries; the limit is %d", len(cfg.Blocklists), azureMaxBlocklists)
	}
	for _, b := range cfg.Blocklists {
		if !azureBlocklistName.MatchString(b) {
			return nil, fmt.Errorf("blocklist name %q must match %s", b, azureBlocklistName.String())
		}
	}
	if v, present := obj["prompt_shields"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("prompt_shields must be a boolean")
		}
		cfg.PromptShield = b
	}
	if len(cfg.Thresholds) == 0 && len(cfg.Blocklists) == 0 && !cfg.PromptShield {
		return nil, fmt.Errorf("a %s detector needs categories, blocklists or prompt_shields", TypeAzureContentSafety)
	}
	d := &azureDetector{cfg: cfg}
	for name := range cfg.Thresholds {
		d.categories = append(d.categories, name)
	}
	sort.Strings(d.categories)
	return d, nil
}

func (c AzureConfig) maxSeverity() int {
	if c.OutputType == azureOutputEight {
		return 7
	}
	return 6
}

// AzureAnalyzeRequest is the text:analyze request body.
type AzureAnalyzeRequest struct {
	Text           string   `json:"text"`
	Categories     []string `json:"categories,omitempty"`
	BlocklistNames []string `json:"blocklistNames,omitempty"`
	OutputType     string   `json:"outputType,omitempty"`
}

// AzureAnalyzeResponse is the text:analyze response body. The blocklist item
// text is not decoded: it is the matched term.
type AzureAnalyzeResponse struct {
	BlocklistsMatch []struct {
		BlocklistName   string `json:"blocklistName"`
		BlocklistItemID string `json:"blocklistItemId"`
	} `json:"blocklistsMatch"`
	CategoriesAnalysis []struct {
		Category string `json:"category"`
		Severity *int   `json:"severity"`
	} `json:"categoriesAnalysis"`
}

// AzureShieldRequest is the text:shieldPrompt request body.
type AzureShieldRequest struct {
	UserPrompt string   `json:"userPrompt,omitempty"`
	Documents  []string `json:"documents,omitempty"`
}

// AzureShieldResponse is the text:shieldPrompt response body.
type AzureShieldResponse struct {
	UserPromptAnalysis *struct {
		AttackDetected bool `json:"attackDetected"`
	} `json:"userPromptAnalysis"`
	DocumentsAnalysis []struct {
		AttackDetected bool `json:"attackDetected"`
	} `json:"documentsAnalysis"`
}

// textLimit is the Azure limit on one text: 10 000 characters. The pipeline
// sends units of at most this size (units.go).
func (d *azureDetector) textLimit() int { return azureMaxCodePoints }

func (d *azureDetector) detect(ctx context.Context, env Env, stage, content string) ([]Finding, error) {
	// A model answer is not a user prompt. Prompt Shields reads it as a
	// document, the shape for indirect attacks.
	return d.check(ctx, env, content, stage == StageOutput)
}

// detectToolResults reads a unit of tool results. Prompt Shields reads it
// as a document (document attack detection): a tool result is data that
// reached the model, not a prompt that the user wrote.
func (d *azureDetector) detectToolResults(ctx context.Context, env Env, _ string, content string) ([]Finding, error) {
	return d.check(ctx, env, content, true)
}

// check runs text:analyze and text:shieldPrompt on one text. The two calls
// run in parallel and share the deadline of ctx. Every finding covers the
// whole text: neither operation returns an offset.
func (d *azureDetector) check(ctx context.Context, env Env, text string, document bool) ([]Finding, error) {
	key, err := resolveSecret(ctx, env, "api_key_secret", d.cfg.APIKeySecret)
	if err != nil {
		return nil, err
	}
	auth := func(r *http.Request) error { r.Header.Set(azureKeyHeader, key); return nil }
	runAnalyze := len(d.categories) > 0 || len(d.cfg.Blocklists) > 0
	var (
		wg                    sync.WaitGroup
		analyzed              []labelScore
		attack                bool
		analyzeErr, shieldErr error
	)
	if d.cfg.PromptShield {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attack, shieldErr = d.shield(ctx, env, text, document, auth)
		}()
	}
	if runAnalyze {
		analyzed, analyzeErr = d.analyze(ctx, env, text, auth)
	}
	wg.Wait()
	if analyzeErr != nil {
		return nil, analyzeErr
	}
	if shieldErr != nil {
		return nil, shieldErr
	}
	out := make([]Finding, 0, len(analyzed)+1)
	for _, l := range analyzed {
		out = append(out, Finding{Whole: true, Label: l.label, Score: l.score})
	}
	if attack {
		out = append(out, Finding{Whole: true, Label: SlotPromptInjection, Score: 1})
	}
	return out, nil
}

type labelScore struct {
	label string
	score float64
}

func (d *azureDetector) analyze(ctx context.Context, env Env, text string, auth func(*http.Request) error) ([]labelScore, error) {
	body, err := json.Marshal(AzureAnalyzeRequest{
		Text: text, Categories: d.categories, BlocklistNames: d.cfg.Blocklists, OutputType: d.cfg.OutputType,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", errAdapter)
	}
	raw, err := egressPost(ctx, env, d.cfg.Endpoint+"/contentsafety/text:analyze?api-version="+azureAPIVersion, body, auth)
	if err != nil {
		return nil, err
	}
	var resp AzureAnalyzeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: response is not the text:analyze shape", errAdapter)
	}
	maxSeverity := d.cfg.maxSeverity()
	var out []labelScore
	for _, c := range resp.CategoriesAnalysis {
		threshold, asked := d.cfg.Thresholds[c.Category]
		if !asked || c.Severity == nil {
			continue
		}
		sev := *c.Severity
		if sev < 0 || sev > 7 {
			return nil, fmt.Errorf("%w: a severity is outside 0..7", errAdapter)
		}
		if sev >= threshold {
			score := float64(sev) / float64(maxSeverity)
			if score > 1 {
				score = 1
			}
			out = append(out, labelScore{azureCategoryLabels[c.Category], score})
		}
	}
	if len(resp.BlocklistsMatch) > maxAdapterFindings {
		return nil, fmt.Errorf("%w: response has more than %d blocklist matches", errAdapter, maxAdapterFindings)
	}
	if len(resp.BlocklistsMatch) > 0 {
		out = append(out, labelScore{LabelBlocklist, 1})
	}
	return out, nil
}

// shield runs Prompt Shields on text: as the userPrompt, or as one entry of
// documents when document is true.
func (d *azureDetector) shield(ctx context.Context, env Env, text string, document bool, auth func(*http.Request) error) (bool, error) {
	req := AzureShieldRequest{UserPrompt: text}
	if document {
		req = AzureShieldRequest{Documents: []string{text}}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("%w: encode request", errAdapter)
	}
	raw, err := egressPost(ctx, env, d.cfg.Endpoint+"/contentsafety/text:shieldPrompt?api-version="+azureAPIVersion, body, auth)
	if err != nil {
		return false, err
	}
	var resp AzureShieldResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false, fmt.Errorf("%w: response is not the text:shieldPrompt shape", errAdapter)
	}
	if resp.UserPromptAnalysis != nil && resp.UserPromptAnalysis.AttackDetected {
		return true, nil
	}
	for _, doc := range resp.DocumentsAnalysis {
		if doc.AttackDetected {
			return true, nil
		}
	}
	return false, nil
}
