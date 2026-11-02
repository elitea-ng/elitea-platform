package guardrail

// The presidio adapter (ADR-0043 decision 1). It sends the content to a
// Microsoft Presidio Analyzer that the customer runs, and reads the PII
// entity spans back.
//
// The request is the Analyzer REST call `POST /analyze`:
//
//	{"text": "...", "language": "en", "entities": ["EMAIL_ADDRESS"], "score_threshold": 0.5}
//
// The answer is a list of recognizer results:
//
//	[{"entity_type": "EMAIL_ADDRESS", "start": 12, "end": 29, "score": 1.0, ...}]
//
// Presidio is Python: start and end are string indices, which are Unicode
// code points. The adapter converts them to byte offsets. The label of a
// finding is the entity type in lower case (`email_address`, `us_ssn`).
// Presidio returns spans, so `mask` replaces only the PII.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TypePresidio is the detector type of the Presidio Analyzer adapter.
const TypePresidio = "presidio"

// DefaultPresidioLanguage is the language the adapter sends when the row
// names none.
const DefaultPresidioLanguage = "en"

// maxPresidioEntities bounds the entities list.
const maxPresidioEntities = 64

var (
	presidioEntityPattern   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	presidioLanguagePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)
)

type presidioDetector struct {
	ep       selfHostedEndpoint
	language string
	entities []string
	minScore float64
}

func newPresidioDetector(obj map[string]any) (detectorImpl, error) {
	ep, err := parseSelfHostedEndpoint(obj, "Authorization", "Bearer ")
	if err != nil {
		return nil, err
	}
	d := &presidioDetector{ep: ep, language: DefaultPresidioLanguage}
	if v, present := obj["language"]; present && v != nil {
		s, ok := v.(string)
		if !ok || !presidioLanguagePattern.MatchString(s) {
			return nil, errors.New("language must be a language code such as en or de")
		}
		d.language = s
	}
	entities, err := stringList(obj, "entities")
	if err != nil {
		return nil, err
	}
	if len(entities) > maxPresidioEntities {
		return nil, fmt.Errorf("entities lists %d entries; the limit is %d", len(entities), maxPresidioEntities)
	}
	for _, e := range entities {
		if !presidioEntityPattern.MatchString(e) {
			return nil, fmt.Errorf("entities entry %q must be a Presidio entity type such as EMAIL_ADDRESS", e)
		}
	}
	d.entities = entities
	if v, present := obj["min_score"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 0 || f > 1 {
			return nil, errors.New("min_score must be a number between 0 and 1")
		}
		d.minScore = f
	}
	return d, nil
}

type presidioRequest struct {
	Text           string   `json:"text"`
	Language       string   `json:"language"`
	Entities       []string `json:"entities,omitempty"`
	ScoreThreshold *float64 `json:"score_threshold,omitempty"`
}

type presidioResult struct {
	EntityType string   `json:"entity_type"`
	Start      *int     `json:"start"`
	End        *int     `json:"end"`
	Score      *float64 `json:"score"`
}

func (d *presidioDetector) detect(ctx context.Context, env Env, _ string, content string) ([]Finding, error) {
	body := presidioRequest{Text: content, Language: d.language, Entities: d.entities}
	if d.minScore > 0 {
		body.ScoreThreshold = &d.minScore
	}
	raw, err := d.ep.post(ctx, env, body)
	if err != nil {
		return nil, err
	}
	var results []presidioResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("%w: response is not a Presidio result list", errAdapter)
	}
	if len(results) > maxAdapterFindings {
		return nil, fmt.Errorf("%w: response has more than %d findings", errAdapter, maxAdapterFindings)
	}
	return d.findings(results, content)
}

// findings converts Presidio results (code point spans) to byte spans.
func (d *presidioDetector) findings(results []presidioResult, content string) ([]Finding, error) {
	var offsets []int
	out := make([]Finding, 0, len(results))
	for _, r := range results {
		if r.Start == nil || r.End == nil {
			return nil, fmt.Errorf("%w: a result has no span", errAdapter)
		}
		label := strings.ToLower(r.EntityType)
		if !ValidLabel(label) {
			return nil, fmt.Errorf("%w: a result entity type is not valid", errAdapter)
		}
		if len(d.entities) > 0 && !containsString(d.entities, r.EntityType) {
			continue
		}
		score := 1.0
		if r.Score != nil {
			score = *r.Score
			if score < 0 || score > 1 {
				return nil, fmt.Errorf("%w: a result score is outside 0..1", errAdapter)
			}
		}
		if score < d.minScore {
			continue
		}
		if offsets == nil {
			offsets = codePointOffsets(content)
		}
		s, e := *r.Start, *r.End
		if s < 0 || e < s || e > len(offsets)-1 {
			return nil, fmt.Errorf("%w: a result span is outside the content", errAdapter)
		}
		if s == e {
			continue
		}
		out = append(out, Finding{Start: offsets[s], End: offsets[e], Label: label, Score: score})
	}
	return out, nil
}
