package httpaction

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"unicode/utf8"
)

// NodeSchema is the `type: http`, `revision: 2` node shape
// (libs/jsonschema/runtime/v1/http-action-node.schema.json).
const NodeSchema = "elitea.pipeline.http-action-node.v2"

// Node is one parsed HTTP node. TemplateWire is the canonical JSON of the
// parts Main renders from ({operation, request, response, timeout_ms?}); it is
// what the start snapshot freezes.
type Node struct {
	ID           string
	Template     NodeTemplate
	TemplateWire []byte
	Output       string
	Transition   *string
}

var operationPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// ParseNode decodes one HTTP node from strict JSON (the YAML node converted
// token by token). Unknown fields, a URL, a method or a credential are refused
// by shape.
func ParseNode(raw []byte) (Node, error) {
	fields, err := strictObject(raw, "id", "type", "revision", "operation", "request", "response", "timeout_ms", "output", "transition")
	if err != nil {
		return Node{}, err
	}
	for _, required := range []string{"id", "type", "revision", "operation", "request", "response", "output"} {
		if _, found := fields[required]; !found {
			return Node{}, ErrInvalid
		}
	}
	var node Node
	var kind, operation string
	var revision json.Number
	var output []string
	if decodeStrict(fields["id"], &node.ID) != nil || !validFrozenID(node.ID) ||
		decodeStrict(fields["type"], &kind) != nil || kind != "http" ||
		decodeStrict(fields["revision"], &revision) != nil || revision.String() != "2" ||
		decodeStrict(fields["operation"], &operation) != nil || len(operation) > maxRuleNameBytes || !operationPattern.MatchString(operation) ||
		decodeStrict(fields["output"], &output) != nil || len(output) != 1 || !validFrozenOutput(output[0]) {
		return Node{}, ErrInvalid
	}
	node.Output = output[0]
	if raw, found := fields["transition"]; found {
		var transition string
		if decodeStrict(raw, &transition) != nil || !validFrozenID(transition) {
			return Node{}, ErrInvalid
		}
		node.Transition = &transition
	}
	request, err := parseRequestTemplate(fields["request"])
	if err != nil {
		return Node{}, err
	}
	if err := validResponseTemplate(fields["response"]); err != nil {
		return Node{}, err
	}
	template := map[string]any{"operation": operation}
	for _, name := range []string{"request", "response", "timeout_ms"} {
		if value, found := fields[name]; found {
			decoded, err := decodeTemplateValue(value)
			if err != nil {
				return Node{}, ErrInvalid
			}
			template[name] = decoded
		}
	}
	if raw, found := fields["timeout_ms"]; found {
		var timeout json.Number
		if decodeStrict(raw, &timeout) != nil || !integerToken.MatchString(timeout.String()) {
			return Node{}, ErrInvalid
		}
		if value, err := timeout.Int64(); err != nil || value < 1 || value > 30000 {
			return Node{}, ErrInvalid
		}
	}
	node.TemplateWire = canonicalValue(template)
	if len(node.TemplateWire) > MaxTemplateBytes {
		return Node{}, ErrInvalid
	}
	node.Template, err = DecodeTemplate(node.TemplateWire)
	if err != nil {
		return Node{}, err
	}
	node.Template.Request = request
	return node, nil
}

// DecodeTemplate reads a frozen template wire back into a NodeTemplate.
func DecodeTemplate(wire []byte) (NodeTemplate, error) {
	if len(wire) == 0 || len(wire) > MaxTemplateBytes {
		return NodeTemplate{}, ErrInvalid
	}
	fields, err := strictObject(wire, "operation", "request", "response", "timeout_ms")
	if err != nil {
		return NodeTemplate{}, err
	}
	var template NodeTemplate
	if decodeStrict(fields["operation"], &template.Operation) != nil || !operationPattern.MatchString(template.Operation) {
		return NodeTemplate{}, ErrInvalid
	}
	if template.Request, err = parseRequestTemplate(fields["request"]); err != nil {
		return NodeTemplate{}, err
	}
	if validResponseTemplate(fields["response"]) != nil {
		return NodeTemplate{}, ErrInvalid
	}
	template.Response = bytes.Clone(fields["response"])
	if raw, found := fields["timeout_ms"]; found {
		template.TimeoutMS = bytes.Clone(raw)
	}
	return template, nil
}

func parseRequestTemplate(raw json.RawMessage) (RequestTemplate, error) {
	fields, err := strictObject(raw, "path", "query", "headers", "body", "idempotency_key")
	if err != nil {
		return RequestTemplate{}, err
	}
	var template RequestTemplate
	if template.Path, err = parseBindingMap(fields["path"]); err != nil {
		return RequestTemplate{}, err
	}
	if template.Query, err = parseBindingMap(fields["query"]); err != nil {
		return RequestTemplate{}, err
	}
	if headers, found := fields["headers"]; found {
		var list []json.RawMessage
		if decodeStrict(headers, &list) != nil || list == nil || len(list) > 32 {
			return RequestTemplate{}, ErrInvalid
		}
		for _, entry := range list {
			header, err := parseHeaderBinding(entry)
			if err != nil {
				return RequestTemplate{}, err
			}
			template.Headers = append(template.Headers, header)
		}
	}
	if body, found := fields["body"]; found {
		parsed, err := parseBodyTemplate(body)
		if err != nil {
			return RequestTemplate{}, err
		}
		template.Body = &parsed
	}
	if key, found := fields["idempotency_key"]; found {
		binding, err := parseBinding(key, false)
		if err != nil {
			return RequestTemplate{}, err
		}
		template.IdempotencyKey = &binding
	}
	return template, nil
}

func parseBindingMap(raw json.RawMessage) (map[string]Binding, error) {
	if raw == nil {
		return map[string]Binding{}, nil
	}
	var entries map[string]json.RawMessage
	if decodeStrict(raw, &entries) != nil || entries == nil || len(entries) > MaxPathPlaceholders {
		return nil, ErrInvalid
	}
	out := make(map[string]Binding, len(entries))
	for name, value := range entries {
		if !bindingKey.MatchString(name) {
			return nil, ErrInvalid
		}
		binding, err := parseBinding(value, false)
		if err != nil {
			return nil, err
		}
		out[name] = binding
	}
	return out, nil
}

// parseBinding reads `{from, optional?}` or `{value}`; literal values are a
// string of at most 1024 bytes, an int64 or a boolean.
func parseBinding(raw json.RawMessage, allowName bool) (Binding, error) {
	allowed := []string{"from", "optional", "value"}
	if allowName {
		allowed = append(allowed, "name")
	}
	fields, err := strictObject(raw, allowed...)
	if err != nil {
		return Binding{}, err
	}
	from, hasFrom := fields["from"]
	value, hasValue := fields["value"]
	optional, hasOptional := fields["optional"]
	var binding Binding
	switch {
	case hasFrom && !hasValue:
		if decodeStrict(from, &binding.From) != nil || !stateKeyPattern.MatchString(binding.From) {
			return Binding{}, ErrInvalid
		}
		if hasOptional && decodeStrict(optional, &binding.Optional) != nil {
			return Binding{}, ErrInvalid
		}
	case hasValue && !hasFrom && !hasOptional:
		decoded, err := decodeTemplateValue(value)
		if err != nil {
			return Binding{}, ErrInvalid
		}
		switch typed := decoded.(type) {
		case string:
			if len(typed) > maxLiteralString {
				return Binding{}, ErrInvalid
			}
		case bool:
		case json.Number:
			if _, err := scalarText(typed); err != nil {
				return Binding{}, ErrInvalid
			}
		default:
			return Binding{}, ErrInvalid
		}
		binding.Value = canonicalValue(decoded)
	default:
		return Binding{}, ErrInvalid
	}
	return binding, nil
}

func parseHeaderBinding(raw json.RawMessage) (HeaderBinding, error) {
	fields, err := strictObject(raw, "name", "from", "optional", "value")
	if err != nil {
		return HeaderBinding{}, err
	}
	var header HeaderBinding
	if decodeStrict(fields["name"], &header.Name) != nil || !headerNameSyntax.MatchString(header.Name) {
		return HeaderBinding{}, ErrInvalid
	}
	if value, found := fields["value"]; found {
		var text string
		if decodeStrict(value, &text) != nil || len(text) > 8192 || bytes.ContainsAny([]byte(text), "\x00\r\n") {
			return HeaderBinding{}, ErrInvalid
		}
	}
	header.Binding, err = parseBinding(raw, true)
	if err != nil {
		return HeaderBinding{}, err
	}
	return header, nil
}

func parseBodyTemplate(raw json.RawMessage) (BodyTemplate, error) {
	fields, err := strictObject(raw, "kind", "from", "value", "content_type")
	if err != nil {
		return BodyTemplate{}, err
	}
	var body BodyTemplate
	if decodeStrict(fields["kind"], &body.Kind) != nil {
		return BodyTemplate{}, ErrInvalid
	}
	from, hasFrom := fields["from"]
	value, hasValue := fields["value"]
	contentType, hasContentType := fields["content_type"]
	if hasContentType && (decodeStrict(contentType, &body.ContentType) != nil || !validMedia(body.ContentType)) {
		return BodyTemplate{}, ErrInvalid
	}
	if hasFrom && (decodeStrict(from, &body.From) != nil || !stateKeyPattern.MatchString(body.From)) {
		return BodyTemplate{}, ErrInvalid
	}
	switch body.Kind {
	case "empty":
		if hasFrom || hasValue || hasContentType {
			return BodyTemplate{}, ErrInvalid
		}
	case "json", "text":
		if hasFrom == hasValue {
			return BodyTemplate{}, ErrInvalid
		}
		if hasValue {
			if !boundedJSON(value) {
				return BodyTemplate{}, ErrInvalid
			}
			decoded, err := decodeTemplateValue(value)
			if err != nil {
				return BodyTemplate{}, ErrInvalid
			}
			body.Value, body.HasValue = canonicalValue(decoded), true
		}
	case "artifact":
		if !hasFrom || hasValue || !hasContentType {
			return BodyTemplate{}, ErrInvalid
		}
	default:
		return BodyTemplate{}, ErrInvalid
	}
	return body, nil
}

func validResponseTemplate(raw json.RawMessage) error {
	fields, err := strictObject(raw, "mode", "accepted_statuses", "max_bytes")
	if err != nil {
		return err
	}
	var contract ResponseContract
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if _, found := fields["mode"]; !found || decoder.Decode(&contract) != nil {
		return ErrInvalid
	}
	if value, found := fields["max_bytes"]; found && (string(value) == "0" || contract.MaxBytes > MaxResponse) {
		return ErrInvalid
	}
	if value, found := fields["accepted_statuses"]; found && (string(value) == "null" || len(contract.AcceptedStatuses) == 0) {
		return ErrInvalid
	}
	probe := Request{Method: "GET", URL: "https://validation.invalid/", Response: contract}
	if probe.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

// strictObject decodes a bounded JSON object and refuses any field not named.
func strictObject(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || !utf8.Valid(raw) || !strictJSON(raw, 40, 20000) {
		return nil, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrInvalid
	}
	for name := range fields {
		known := false
		for _, candidate := range allowed {
			if name == candidate {
				known = true
				break
			}
		}
		if !known {
			return nil, ErrInvalid
		}
	}
	return fields, nil
}

func decodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}
