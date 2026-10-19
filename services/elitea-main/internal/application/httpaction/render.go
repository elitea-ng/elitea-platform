package httpaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Render limits (§3.4 of the T-5D brief). MaxInputsBytes is mirrored by the
// Worker's MAX_HTTP_INPUTS_BYTES; parity is pinned by the render vectors.
const (
	// MaxInputsBytes bounds the canonical inputs the Worker sends (decoded).
	MaxInputsBytes = 256 * 1024
	// MaxSegmentBytes bounds one rendered path segment before encoding.
	MaxSegmentBytes = 1024
	// MaxTemplateBytes bounds one frozen node template (canonical JSON).
	MaxTemplateBytes = 64 * 1024
	maxLiteralString = 1024
)

// Failure codes a render or admission refusal carries. They are the only
// detail a caller (and the Worker) sees.
const (
	CodeInvalidInput  = "invalid_input"
	CodePolicyDenied  = "policy_denied"
	CodeInputsTooBig  = "inputs_too_large"
	CodeNonDetermined = "nondeterministic_inputs"
)

// Failure is a typed refusal with a stable code. invalid_input and
// inputs_too_large wrap ErrInvalid; policy_denied wraps ErrUnauthorized;
// nondeterministic_inputs wraps ErrNonDeterministic.
type Failure struct{ Code string }

func (f *Failure) Error() string { return "HTTP action refused: " + f.Code }
func (f *Failure) Unwrap() error {
	switch f.Code {
	case CodePolicyDenied:
		return ErrUnauthorized
	case CodeNonDetermined:
		return ErrNonDeterministic
	default:
		return ErrInvalid
	}
}

// ErrNonDeterministic: the same activation arrived with different inputs.
var ErrNonDeterministic = errors.New("HTTP action inputs differ from the recorded activation")

// FailureCode returns the stable code of err, or "" when err is not a Failure.
func FailureCode(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func invalidInput() error { return &Failure{Code: CodeInvalidInput} }
func policyDenied() error { return &Failure{Code: CodePolicyDenied} }

// Binding is `{from: key, optional?: bool}` or `{value: string|integer|boolean}`.
type Binding struct {
	From     string
	Optional bool
	// Value is the canonical literal (a JSON string, integer or boolean token).
	Value json.RawMessage
}

// HeaderBinding is `{name, from, optional?}` or `{name, value}`.
type HeaderBinding struct {
	Name string
	Binding
}

// BodyTemplate is `{kind: empty}`, `{kind: json|text, from|value, content_type?}`
// or `{kind: artifact, from, content_type}`.
type BodyTemplate struct {
	Kind        string
	From        string
	Value       json.RawMessage
	HasValue    bool
	ContentType string
}

// RequestTemplate is the node's `request` object.
type RequestTemplate struct {
	Path           map[string]Binding
	Query          map[string]Binding
	Headers        []HeaderBinding
	Body           *BodyTemplate
	IdempotencyKey *Binding
}

// NodeTemplate is the frozen part of one `type: http`, `revision: 2` node that
// Main renders from. It never contains a URL, method or credential.
type NodeTemplate struct {
	Operation string
	Request   RequestTemplate
	// Response and TimeoutMS are copied into the request wire verbatim.
	Response  json.RawMessage
	TimeoutMS json.RawMessage
}

var (
	stateKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	bindingKey       = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,64}$`)
	headerNameSyntax = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,128}$")
	integerToken     = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,18})$`)
)

// Inputs is the decoded canonical inputs object the Worker sends. Only
// bindings with `from` have an entry.
type Inputs struct {
	Path           map[string]any
	Query          map[string]any
	Headers        map[string]any
	Body           any
	HasBody        bool
	IdempotencyKey *string
}

// DecodeInputs checks the inputs bytes are within MaxInputsBytes, strict and
// canonical, and returns them decoded. Non-canonical bytes are refused so the
// recorded inputs_digest identifies exactly one value.
func DecodeInputs(raw []byte) (Inputs, error) {
	if len(raw) > MaxInputsBytes {
		return Inputs{}, &Failure{Code: CodeInputsTooBig}
	}
	if len(raw) == 0 {
		return Inputs{}, invalidInput()
	}
	canonical, err := CanonicalJSON(raw)
	if err != nil || !bytes.Equal(canonical, raw) {
		return Inputs{}, invalidInput()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil || fields == nil {
		return Inputs{}, invalidInput()
	}
	var inputs Inputs
	for name, value := range fields {
		switch name {
		case "path", "query", "headers":
			object, ok := value.(map[string]any)
			if !ok || len(object) > MaxPathPlaceholders {
				return Inputs{}, invalidInput()
			}
			switch name {
			case "path":
				inputs.Path = object
			case "query":
				inputs.Query = object
			default:
				inputs.Headers = object
			}
		case "body":
			inputs.Body, inputs.HasBody = value, true
		case "idempotency_key":
			text, ok := value.(string)
			if !ok {
				return Inputs{}, invalidInput()
			}
			inputs.IdempotencyKey = &text
		default:
			return Inputs{}, invalidInput()
		}
	}
	return inputs, nil
}

// Rendered is one deterministic request.
type Rendered struct {
	Request Request
	// Wire is the canonical request wire; Digest is its SHA-256.
	Wire    []byte
	Digest  string
	Path    string
	Query   string
	Headers []Header
	// Body is the exact body bytes for json/text (nil for empty and artifact).
	Body []byte
}

// Render builds the request for rule from the frozen node template and the
// canonical inputs. Shape mismatches between node and rule are policy_denied;
// bad or missing input values are invalid_input. It performs no I/O.
func Render(rule Rule, node NodeTemplate, rawInputs []byte) (Rendered, error) {
	inputs, err := DecodeInputs(rawInputs)
	if err != nil {
		return Rendered{}, err
	}
	if rule.Validate() != nil || !rule.Enabled || node.Operation != rule.Name {
		return Rendered{}, policyDenied()
	}
	path, err := renderPath(rule, node.Request.Path, inputs.Path)
	if err != nil {
		return Rendered{}, err
	}
	query, err := renderQuery(rule, node.Request.Query, inputs.Query)
	if err != nil {
		return Rendered{}, err
	}
	headers, err := renderHeaders(node.Request.Headers, inputs.Headers)
	if err != nil {
		return Rendered{}, err
	}
	body, bodyBytes, err := renderBody(rule.Method, node.Request.Body, inputs)
	if err != nil {
		return Rendered{}, err
	}
	key, err := renderIdempotencyKey(node.Request.IdempotencyKey, inputs.IdempotencyKey)
	if err != nil {
		return Rendered{}, err
	}
	url := rule.Origin + path
	if query != "" {
		url += "?" + query
	}
	wire := map[string]any{"method": rule.Method, "url": url}
	if rule.ConfigurationID > 0 {
		wire["credential"] = map[string]any{"configuration_id": ruleNumber(int64(rule.ConfigurationID))}
	}
	if len(headers) > 0 {
		list := make([]any, len(headers))
		for i, header := range headers {
			list[i] = map[string]any{"name": header.Name, "value": header.Value}
		}
		wire["headers"] = list
	}
	if body != nil {
		wire["body"] = body
	}
	if key != "" {
		wire["idempotency_key"] = key
	}
	response, err := decodeTemplateValue(node.Response)
	if err != nil {
		return Rendered{}, invalidInput()
	}
	wire["response"] = response
	if len(node.TimeoutMS) > 0 {
		timeout, err := decodeTemplateValue(node.TimeoutMS)
		if err != nil {
			return Rendered{}, invalidInput()
		}
		wire["timeout_ms"] = timeout
	}
	encoded := canonicalValue(wire)
	if len(encoded) > MaxRequest {
		return Rendered{}, invalidInput()
	}
	var request Request
	if Decode(encoded, &request) != nil || request.Validate() != nil {
		return Rendered{}, invalidInput()
	}
	return Rendered{Request: request, Wire: encoded, Digest: Digest(encoded), Path: path, Query: query, Headers: headers, Body: bodyBytes}, nil
}

func renderPath(rule Rule, bindings map[string]Binding, inputs map[string]any) (string, error) {
	placeholders, err := rule.Placeholders()
	if err != nil {
		return "", policyDenied()
	}
	if len(bindings) != len(placeholders) {
		return "", policyDenied()
	}
	values := make(map[string]string, len(placeholders))
	used := 0
	for _, name := range placeholders {
		binding, found := bindings[name]
		if !found {
			return "", policyDenied()
		}
		raw, present, err := bindingValue(name, binding, inputs)
		if err != nil {
			return "", err
		}
		if binding.From != "" && present {
			used++
		}
		if !present {
			return "", invalidInput()
		}
		text, err := scalarText(raw)
		if err != nil {
			return "", err
		}
		if text == "" || text == "." || text == ".." || len(text) > MaxSegmentBytes {
			return "", invalidInput()
		}
		values[name] = encodeComponent(text)
	}
	if used != len(inputs) {
		return "", invalidInput()
	}
	if rule.PathTemplate == "/" {
		return "/", nil
	}
	var out strings.Builder
	for _, segment := range strings.Split(rule.PathTemplate[1:], "/") {
		out.WriteByte('/')
		if match := placeholderSegment.FindStringSubmatch(segment); match != nil {
			out.WriteString(values[match[1]])
		} else {
			out.WriteString(segment)
		}
	}
	return out.String(), nil
}

func renderQuery(rule Rule, bindings map[string]Binding, inputs map[string]any) (string, error) {
	allowed := make(map[string]bool, len(rule.QueryNames))
	for _, name := range rule.QueryNames {
		allowed[name] = true
	}
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		if !allowed[name] {
			return "", policyDenied()
		}
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	used := 0
	for _, name := range names {
		binding := bindings[name]
		raw, present, err := bindingValue(name, binding, inputs)
		if err != nil {
			return "", err
		}
		if !present {
			if binding.Optional {
				continue
			}
			return "", invalidInput()
		}
		if binding.From != "" {
			used++
		}
		text, err := scalarText(raw)
		if err != nil {
			return "", err
		}
		pairs = append(pairs, encodeComponent(name)+"="+encodeComponent(text))
	}
	if used != len(inputs) {
		return "", invalidInput()
	}
	return strings.Join(pairs, "&"), nil
}

func renderHeaders(bindings []HeaderBinding, inputs map[string]any) ([]Header, error) {
	headers := make([]Header, 0, len(bindings))
	seen := make(map[string]bool, len(bindings))
	used := 0
	for _, binding := range bindings {
		name := strings.ToLower(binding.Name)
		if !headerNameSyntax.MatchString(binding.Name) || RestrictedHeader(name) || seen[name] {
			return nil, invalidInput()
		}
		seen[name] = true
		var raw any
		present := false
		if binding.From != "" {
			raw, present = inputs[name]
			if present {
				used++
			}
		} else {
			value, err := decodeTemplateValue(binding.Value)
			if err != nil {
				return nil, invalidInput()
			}
			raw, present = value, true
		}
		if !present {
			if binding.Optional {
				continue
			}
			return nil, invalidInput()
		}
		text, err := scalarText(raw)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(text, "\r\n\x00") {
			return nil, invalidInput()
		}
		headers = append(headers, Header{Name: name, Value: text})
	}
	if used != len(inputs) {
		return nil, invalidInput()
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].Name < headers[j].Name })
	return headers, nil
}

func renderBody(method string, template *BodyTemplate, inputs Inputs) (map[string]any, []byte, error) {
	if template == nil || template.Kind == "empty" {
		if inputs.HasBody {
			return nil, nil, invalidInput()
		}
		return nil, nil, nil
	}
	if SafeMethod(method) {
		return nil, nil, invalidInput()
	}
	var value any
	if template.From != "" {
		if !inputs.HasBody {
			return nil, nil, invalidInput()
		}
		value = inputs.Body
	} else {
		if inputs.HasBody || !template.HasValue {
			return nil, nil, invalidInput()
		}
		decoded, err := decodeTemplateValue(template.Value)
		if err != nil {
			return nil, nil, invalidInput()
		}
		value = decoded
	}
	body := map[string]any{"kind": template.Kind}
	if template.ContentType != "" {
		body["content_type"] = template.ContentType
	}
	switch template.Kind {
	case "json":
		body["value"] = value
		return body, canonicalValue(value), nil
	case "text":
		text, ok := value.(string)
		if !ok {
			return nil, nil, invalidInput()
		}
		body["value"] = text
		return body, []byte(text), nil
	case "artifact":
		if template.From == "" {
			return nil, nil, invalidInput()
		}
		body["reference"] = value
		return body, nil, nil
	default:
		return nil, nil, invalidInput()
	}
}

func renderIdempotencyKey(binding *Binding, input *string) (string, error) {
	if binding == nil {
		if input != nil {
			return "", invalidInput()
		}
		return "", nil
	}
	if binding.From != "" {
		if input == nil {
			if binding.Optional {
				return "", nil
			}
			return "", invalidInput()
		}
		return *input, nil
	}
	if input != nil {
		return "", invalidInput()
	}
	value, err := decodeTemplateValue(binding.Value)
	if err != nil {
		return "", invalidInput()
	}
	return scalarText(value)
}

// bindingValue resolves a binding from its literal or from inputs[name].
func bindingValue(name string, binding Binding, inputs map[string]any) (any, bool, error) {
	if binding.From == "" {
		value, err := decodeTemplateValue(binding.Value)
		if err != nil {
			return nil, false, invalidInput()
		}
		return value, true, nil
	}
	value, present := inputs[name]
	return value, present, nil
}

// scalarText renders a string, integer or boolean. Floats, null, arrays and
// objects are refused.
func scalarText(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case json.Number:
		text := typed.String()
		if !integerToken.MatchString(text) {
			return "", invalidInput()
		}
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return "", invalidInput()
		}
		return text, nil
	default:
		return "", invalidInput()
	}
}

// encodeComponent percent-encodes everything except the unreserved set
// A-Z a-z 0-9 - . _ ~ with uppercase hex (the Worker's encode_component set).
// '/' becomes %2F and space %20, so a value stays one segment or one pair.
func encodeComponent(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[c>>4])
		out.WriteByte(hexDigits[c&0xf])
	}
	return out.String()
}

func decodeTemplateValue(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	return value, nil
}

func ruleNumber(value int64) json.Number { return json.Number(strconv.FormatInt(value, 10)) }
