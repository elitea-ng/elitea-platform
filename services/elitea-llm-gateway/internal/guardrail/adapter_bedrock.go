package guardrail

// The AWS Bedrock Guardrails adapter (ADR-0043 decision 1). adapter_cloud.go
// describes the envelope that all cloud adapters share.
//
// API: ApplyGuardrail,
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ApplyGuardrail.html
//
//	POST {endpoint}/guardrail/{guardrailIdentifier}/version/{guardrailVersion}/apply
//	{"source": "INPUT" | "OUTPUT", "content": [{"text": {"text": "..."}}]}
//
// The source comes from the stage of the row. The endpoint is
// https://bedrock-runtime.{region}.amazonaws.com unless the detector names
// one (for example a VPC interface endpoint).
//
// Auth is AWS Signature Version 4 (service "bedrock", the detector's region),
// with the signer the Bedrock connection check uses
// (internal/llmproxy/checkconnection_cloud.go). The access key id, the secret
// access key and the optional session token are {{secret.NAME}} references.
// The gateway does not use its pod identity (IRSA or web identity) here: it
// refuses ambient AWS identity on purpose (issue #454, account/account.go).
//
// Mapping of the assessments to findings:
//
//   - contentPolicy filters: HATE, INSULTS, SEXUAL, VIOLENCE, MISCONDUCT have
//     the lower-case type as label; PROMPT_ATTACK has LabelPromptInjection.
//     Score from the confidence: NONE 0, LOW 0.33, MEDIUM 0.66, HIGH 1.
//   - topicPolicy topics: label "denied_topic", score 1.
//   - wordPolicy: custom words "blocked_word", managed lists "profanity",
//     score 1.
//   - sensitiveInformationPolicy: PII entities "pii:<type>" (for example
//     pii:email), regexes "regex:<name>", score 1.
//   - contextualGroundingPolicy and automatedReasoningPolicy are ignored: a
//     guardrail row has no grounding source.
//
// An entry counts when "detected" is true, or, when the field is absent, when
// its action is not NONE. Bedrock returns the matched text of word and
// sensitive-information entries, not an offset. The adapter finds that text
// in the content to make byte spans, and then drops the text. When the text
// is not found, the finding is Whole. Every other finding is Whole. When
// Bedrock intervenes and no entry maps, the finding is "bedrock_intervened".

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// TypeBedrockGuardrails is the detector type of the AWS Bedrock Guardrails
// adapter.
const TypeBedrockGuardrails = "bedrock_guardrails"

const bedrockSigningService = "bedrock"

var (
	// bedrockRegion is the shape of an AWS region. The region goes into the
	// host name, so the shape is a security control.
	bedrockRegion = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]{1,2}$`)
	// bedrockGuardrailID is the AWS pattern of guardrailIdentifier: an id or
	// a guardrail ARN.
	bedrockGuardrailID = regexp.MustCompile(`^(?:[a-z0-9]{1,128}|arn:aws(?:-[^:]+)?:bedrock:[a-z0-9-]{1,20}:[0-9]{12}:guardrail/[a-z0-9]{1,128})$`)
	// bedrockGuardrailVersion is the AWS pattern of guardrailVersion.
	bedrockGuardrailVersion = regexp.MustCompile(`^(?:[1-9][0-9]{0,7}|DRAFT)$`)
)

// BedrockConfig is the authored configuration of a bedrock_guardrails
// detector.
type BedrockConfig struct {
	Region              string
	GuardrailIdentifier string
	GuardrailVersion    string
	// Endpoint is the bedrock-runtime origin.
	Endpoint              string
	AccessKeyIDSecret     string
	SecretAccessKeySecret string
	SessionTokenSecret    string
}

type bedrockDetector struct{ cfg BedrockConfig }

func newBedrockDetector(obj map[string]any) (detectorImpl, error) {
	var cfg BedrockConfig
	cfg.Region, _ = obj["region"].(string)
	if !bedrockRegion.MatchString(cfg.Region) {
		return nil, errors.New("region must be an AWS region name, for example us-east-1")
	}
	cfg.GuardrailIdentifier, _ = obj["guardrail_identifier"].(string)
	if !bedrockGuardrailID.MatchString(cfg.GuardrailIdentifier) {
		return nil, errors.New("guardrail_identifier must be a guardrail id or a guardrail ARN")
	}
	cfg.GuardrailVersion, _ = obj["guardrail_version"].(string)
	if !bedrockGuardrailVersion.MatchString(cfg.GuardrailVersion) {
		return nil, errors.New(`guardrail_version must be a version number or "DRAFT"`)
	}
	cfg.Endpoint = "https://bedrock-runtime." + cfg.Region + ".amazonaws.com"
	if raw, ok := obj["endpoint"].(string); ok && raw != "" {
		endpoint, err := cloudEndpoint("endpoint", raw)
		if err != nil {
			return nil, err
		}
		cfg.Endpoint = endpoint
	}
	var err error
	if cfg.AccessKeyIDSecret, err = secretRef(obj, "access_key_id_secret"); err != nil {
		return nil, err
	}
	if cfg.SecretAccessKeySecret, err = secretRef(obj, "secret_access_key_secret"); err != nil {
		return nil, err
	}
	if cfg.SessionTokenSecret, err = secretRef(obj, "session_token_secret"); err != nil {
		return nil, err
	}
	if cfg.AccessKeyIDSecret == "" || cfg.SecretAccessKeySecret == "" {
		return nil, errors.New("access_key_id_secret and secret_access_key_secret are required {{secret.NAME}} references")
	}
	return &bedrockDetector{cfg: cfg}, nil
}

// BedrockApplyRequest is the ApplyGuardrail request body.
type BedrockApplyRequest struct {
	Source  string                `json:"source"`
	Content []BedrockContentBlock `json:"content"`
}

// BedrockContentBlock is one text content block.
type BedrockContentBlock struct {
	Text BedrockText `json:"text"`
}

// BedrockText is the text of a content block.
type BedrockText struct {
	Text string `json:"text"`
}

// bedrockEntry is the common shape of an assessment entry.
type bedrockEntry struct {
	Action     string `json:"action"`
	Detected   *bool  `json:"detected"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Match      string `json:"match"`
	Confidence string `json:"confidence"`
}

func (e bedrockEntry) hit() bool {
	if e.Detected != nil {
		return *e.Detected
	}
	return e.Action != "" && e.Action != "NONE"
}

// BedrockApplyResponse is the part of the ApplyGuardrail response the
// adapter reads.
type BedrockApplyResponse struct {
	Action      string `json:"action"`
	Assessments []struct {
		ContentPolicy *struct {
			Filters []bedrockEntry `json:"filters"`
		} `json:"contentPolicy"`
		TopicPolicy *struct {
			Topics []bedrockEntry `json:"topics"`
		} `json:"topicPolicy"`
		WordPolicy *struct {
			CustomWords      []bedrockEntry `json:"customWords"`
			ManagedWordLists []bedrockEntry `json:"managedWordLists"`
		} `json:"wordPolicy"`
		SensitiveInformationPolicy *struct {
			PIIEntities []bedrockEntry `json:"piiEntities"`
			Regexes     []bedrockEntry `json:"regexes"`
		} `json:"sensitiveInformationPolicy"`
	} `json:"assessments"`
}

// bedrockConfidence maps a filter confidence to a score.
func bedrockConfidence(c string) float64 {
	switch c {
	case "HIGH":
		return 1
	case "MEDIUM":
		return 0.66
	case "LOW":
		return 0.33
	case "NONE":
		return 0
	}
	return 1
}

func (d *bedrockDetector) target() string {
	return d.cfg.Endpoint + "/guardrail/" + url.PathEscape(d.cfg.GuardrailIdentifier) +
		"/version/" + d.cfg.GuardrailVersion + "/apply"
}

func (d *bedrockDetector) detect(ctx context.Context, env Env, stage, content string) ([]Finding, error) {
	source := "INPUT"
	if stage == StageOutput {
		source = "OUTPUT"
	}
	body, err := json.Marshal(BedrockApplyRequest{
		Source: source, Content: []BedrockContentBlock{{Text: BedrockText{Text: content}}},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", errAdapter)
	}
	creds := aws.Credentials{}
	if creds.AccessKeyID, err = resolveSecret(ctx, env, "access_key_id_secret", d.cfg.AccessKeyIDSecret); err != nil {
		return nil, err
	}
	if creds.SecretAccessKey, err = resolveSecret(ctx, env, "secret_access_key_secret", d.cfg.SecretAccessKeySecret); err != nil {
		return nil, err
	}
	if d.cfg.SessionTokenSecret != "" {
		if creds.SessionToken, err = resolveSecret(ctx, env, "session_token_secret", d.cfg.SessionTokenSecret); err != nil {
			return nil, err
		}
	}
	creds.AccessKeyID = strings.TrimSpace(creds.AccessKeyID)
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	sign := func(r *http.Request) error {
		r.Header.Set("x-amz-content-sha256", payloadHash)
		if err := v4.NewSigner().SignHTTP(ctx, creds, r, payloadHash, bedrockSigningService, d.cfg.Region, time.Now().UTC()); err != nil {
			return fmt.Errorf("%w: sign request", errAdapter)
		}
		return nil
	}
	raw, err := cloudPost(ctx, env, d.target(), body, sign)
	if err != nil {
		return nil, err
	}
	var resp BedrockApplyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: response is not the ApplyGuardrail shape", errAdapter)
	}
	out := bedrockFindings(&resp, content)
	if len(out) > maxAdapterFindings {
		return nil, fmt.Errorf("%w: response has more than %d findings", errAdapter, maxAdapterFindings)
	}
	return out, nil
}

func bedrockFindings(resp *BedrockApplyResponse, content string) []Finding {
	var out []Finding
	for _, a := range resp.Assessments {
		if a.ContentPolicy != nil {
			for _, f := range a.ContentPolicy.Filters {
				if !f.hit() {
					continue
				}
				label := vendorLabel("", f.Type, "content_filter")
				if f.Type == "PROMPT_ATTACK" {
					label = LabelPromptInjection
				}
				out = append(out, Finding{Whole: true, Label: label, Score: bedrockConfidence(f.Confidence)})
			}
		}
		if a.TopicPolicy != nil {
			for _, t := range a.TopicPolicy.Topics {
				if t.hit() {
					out = append(out, Finding{Whole: true, Label: "denied_topic", Score: 1})
				}
			}
		}
		if a.WordPolicy != nil {
			for _, w := range a.WordPolicy.CustomWords {
				if w.hit() {
					out = append(out, spanFindings(locateAll(content, w.Match, true), "blocked_word", 1)...)
				}
			}
			for _, w := range a.WordPolicy.ManagedWordLists {
				if w.hit() {
					out = append(out, spanFindings(locateAll(content, w.Match, true), "profanity", 1)...)
				}
			}
		}
		if a.SensitiveInformationPolicy != nil {
			for _, p := range a.SensitiveInformationPolicy.PIIEntities {
				if p.hit() {
					out = append(out, spanFindings(locateAll(content, p.Match, false), vendorLabel("pii:", p.Type, "pii"), 1)...)
				}
			}
			for _, r := range a.SensitiveInformationPolicy.Regexes {
				if r.hit() {
					out = append(out, spanFindings(locateAll(content, r.Match, false), vendorLabel("regex:", r.Name, "sensitive_regex"), 1)...)
				}
			}
		}
	}
	if len(out) == 0 && resp.Action == "GUARDRAIL_INTERVENED" {
		out = append(out, Finding{Whole: true, Label: "bedrock_intervened", Score: 1})
	}
	return out
}
