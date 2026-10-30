// Package guardrail is the gateway's content guardrail pipeline (ADR-0043
// decision 1, issue #1500).
//
// A `guardrail` row in gateway.governance_config names a stage (input or
// output), an ordered list of detectors and, per detector, an action (block,
// mask or flag). The gateway runs the rows that select a request on the
// request messages before the provider call (input) and on the model's answer
// (output), streamed or not.
//
// # The detector contract
//
// A detector reads content and returns findings: byte spans, labels and
// scores. It never returns or stores the matched text. A finding with no span
// covers the whole content.
//
// Every detector of a stage reads the ORIGINAL content, not the content a
// previous detector masked. The mask spans of all detectors are merged and
// applied once. This keeps the result independent of how the content arrived
// (one body or many stream chunks), which is what makes the streaming masker
// (stream.go) equal to the non-streamed one.
//
// # Verdicts
//
// Each detector run produces a Verdict: the row, the detector, the labels, the
// action, the outcome, the latency and the fail mode. A Verdict has no field
// that content can reach. The gateway writes verdicts to
// gateway.guardrail_verdicts through the request log (internal/requestlog).
package guardrail

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Stages.
const (
	StageInput  = "input"
	StageOutput = "output"
)

// Actions.
const (
	ActionBlock = "block"
	ActionMask  = "mask"
	ActionFlag  = "flag"
)

// Fail modes.
const (
	FailOpen   = "open"
	FailClosed = "closed"
)

// Detector types.
const (
	TypeRegex   = "regex"
	TypeSecrets = "secrets"
	TypeHTTP    = "http"
)

// Bounds. elitea-main refuses a row outside them on write; the gateway refuses
// one that reached the table another way.
const (
	MaxDetectors       = 16
	MaxPatterns        = 64
	MaxPatternBytes    = 1024
	MaxTerms           = 1000
	MaxTermBytes       = 256
	MaxTermsTotalBytes = 64 << 10
	MaxLabels          = 32

	DefaultTimeout = 2 * time.Second
	MinTimeout     = 50 * time.Millisecond
	MaxTimeout     = 30 * time.Second

	// DefaultStreamWindow is the look-behind buffer of the streaming masker,
	// in bytes. The client sees streamed text at most 2 × window bytes behind
	// the model while a block or mask detector applies (stream.go).
	DefaultStreamWindow = 256
	MinStreamWindow     = 64
	MaxStreamWindow     = 8192
)

// labelPattern bounds a label, from a row or from an adapter response. A label
// goes into the mask marker and the verdict, so it must not carry content.
var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,63}$`)

// ValidLabel reports whether s is a usable label.
func ValidLabel(s string) bool { return labelPattern.MatchString(s) }

// Row is one compiled guardrail row.
type Row struct {
	Name  string
	Stage string
	// Detectors run in this order. A block stops the row.
	Detectors []*Detector
	Timeout   time.Duration
	FailMode  string
	// ApplyToEmbeddings makes an input row inspect embeddings requests too.
	// Output rows never apply to embeddings.
	ApplyToEmbeddings bool
	StreamWindow      int
}

// Detector is one configured detector of a row.
type Detector struct {
	Name   string
	Type   string
	Action string
	impl   detectorImpl
}

// HTTPAdapter returns the adapter configuration of an http detector, or nil.
func (d *Detector) HTTPAdapter() *HTTPConfig {
	if h, ok := d.impl.(*httpDetector); ok {
		return &h.cfg
	}
	return nil
}

// ParseRow compiles the `guardrail` object of a row's data. The object can sit
// under the `guardrail` key (the admin form's shape) or at the top level of
// data (a hand-written row).
func ParseRow(name string, data map[string]any) (*Row, error) {
	g, _ := data["guardrail"].(map[string]any)
	if len(g) == 0 {
		g = data
	}
	row := &Row{Name: name, StreamWindow: DefaultStreamWindow, Timeout: DefaultTimeout}

	row.Stage, _ = g["stage"].(string)
	switch row.Stage {
	case StageInput, StageOutput:
	default:
		return nil, fmt.Errorf("guardrail.stage must be %q or %q", StageInput, StageOutput)
	}

	rawDetectors, ok := g["detectors"].([]any)
	if !ok || len(rawDetectors) == 0 {
		return nil, errors.New("guardrail.detectors must list at least one detector")
	}
	if len(rawDetectors) > MaxDetectors {
		return nil, fmt.Errorf("guardrail.detectors lists %d detectors; the limit is %d", len(rawDetectors), MaxDetectors)
	}
	names := map[string]bool{}
	for i, raw := range rawDetectors {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("guardrail.detectors[%d] must be an object", i)
		}
		d, err := parseDetector(obj)
		if err != nil {
			return nil, fmt.Errorf("guardrail.detectors[%d]: %w", i, err)
		}
		if names[d.Name] {
			return nil, fmt.Errorf("guardrail.detectors[%d]: the name %q is used twice", i, d.Name)
		}
		names[d.Name] = true
		row.Detectors = append(row.Detectors, d)
	}

	if v, present := g["timeout_ms"]; present && v != nil {
		ms, ok := wholeNumber(v)
		if !ok {
			return nil, errors.New("guardrail.timeout_ms must be a whole number")
		}
		row.Timeout = time.Duration(ms) * time.Millisecond
		if row.Timeout < MinTimeout || row.Timeout > MaxTimeout {
			return nil, fmt.Errorf("guardrail.timeout_ms must be between %d and %d",
				MinTimeout.Milliseconds(), MaxTimeout.Milliseconds())
		}
	}

	row.FailMode, _ = g["fail_mode"].(string)
	switch row.FailMode {
	case "":
		row.FailMode = DefaultFailMode(row.Detectors)
	case FailOpen, FailClosed:
	default:
		return nil, fmt.Errorf("guardrail.fail_mode must be %q or %q", FailOpen, FailClosed)
	}

	if v, present := g["apply_to_embeddings"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("guardrail.apply_to_embeddings must be a boolean")
		}
		if b && row.Stage != StageInput {
			return nil, errors.New("guardrail.apply_to_embeddings applies to input rows only: embeddings have no text output")
		}
		row.ApplyToEmbeddings = b
	}

	if v, present := g["stream_window_bytes"]; present && v != nil {
		n, ok := wholeNumber(v)
		if !ok || n < MinStreamWindow || n > MaxStreamWindow {
			return nil, fmt.Errorf("guardrail.stream_window_bytes must be a whole number between %d and %d",
				MinStreamWindow, MaxStreamWindow)
		}
		row.StreamWindow = int(n)
	}
	return row, nil
}

// DefaultFailMode is the fail mode of a row that names none: closed when a
// detector blocks or masks, open when every detector only flags. A mask row
// fails closed because a failed mask would release the raw text.
func DefaultFailMode(detectors []*Detector) string {
	for _, d := range detectors {
		if d.Action != ActionFlag {
			return FailClosed
		}
	}
	return FailOpen
}

func parseDetector(obj map[string]any) (*Detector, error) {
	d := &Detector{}
	d.Type, _ = obj["type"].(string)
	d.Action, _ = obj["action"].(string)
	d.Name, _ = obj["name"].(string)
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		d.Name = d.Type
	}
	if !ValidLabel(d.Name) {
		return nil, fmt.Errorf("name %q must match %s", d.Name, labelPattern.String())
	}
	switch d.Action {
	case ActionBlock, ActionMask, ActionFlag:
	default:
		return nil, fmt.Errorf("action must be %q, %q or %q", ActionBlock, ActionMask, ActionFlag)
	}
	var err error
	switch d.Type {
	case TypeRegex:
		d.impl, err = newRegexDetector(obj)
	case TypeSecrets:
		d.impl, err = newSecretsDetector(obj)
	case TypeHTTP:
		d.impl, err = newHTTPDetector(obj)
	default:
		return nil, fmt.Errorf("type must be %q, %q or %q", TypeRegex, TypeSecrets, TypeHTTP)
	}
	if err != nil {
		return nil, err
	}
	if h, ok := d.impl.(*httpDetector); ok {
		h.name = d.Name
	}
	return d, nil
}

// wholeNumber reads a JSON number (or a numeric string) that is a whole number.
func wholeNumber(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// stringList reads a list of strings. A non-string entry is an error: a
// pattern or term that vanished silently is a rule that does not apply.
func stringList(obj map[string]any, key string) ([]string, error) {
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
