package pipelinelimits

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	// CodeHTTPNodeInvalid is an HTTP Request node (`type: http`) whose YAML
	// shape, bounds, or referenced rule is refused at save or start time.
	CodeHTTPNodeInvalid = "PIPELINE_HTTP_NODE_INVALID"

	httpMaxHeaders          = 32
	httpMaxHeaderNameBytes  = 128
	httpMaxHeaderValueBytes = 8_192
	httpMaxBindingEntries   = 32
	httpMaxAcceptedStatuses = 16
	httpMaxResponseBytes    = 2 * 1024 * 1024
	httpMaxInputKeys        = 64
	httpMaxLiteralBytes     = 1_024
	httpMaxRuleNameBytes    = 128
)

var (
	httpOperationPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	httpStateKeyPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	httpBindingKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,64}$`)
	httpHeaderNamePattern = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,128}$")
	httpMediaTypePattern  = regexp.MustCompile(`^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+-]+$`)
)

// HTTPNodeOperation identifies one `type: http` node and the rule name it invokes.
type HTTPNodeOperation struct {
	NodeID    string
	Operation string
}

// UnknownHTTPOperationRefusal returns a readable 400 refusal when a saved
// pipeline's HTTP node names a rule that does not exist in the project.
func UnknownHTTPOperationRefusal(nodeID, operation string) error {
	return &apierr.APIError{
		Status: http.StatusBadRequest,
		Code:   CodeHTTPNodeInvalid,
		Message: fmt.Sprintf(
			"Node %s references HTTP action rule %s, which does not exist in this project. Create or select an existing rule under Settings → HTTP actions before saving.",
			quoted(nodeID), quoted(operation),
		),
	}
}

// DisabledHTTPOperationRefusal returns a readable 400 refusal when a saved
// pipeline's HTTP node names a rule that is disabled in the project.
func DisabledHTTPOperationRefusal(nodeID, operation string) error {
	return &apierr.APIError{
		Status: http.StatusBadRequest,
		Code:   CodeHTTPNodeInvalid,
		Message: fmt.Sprintf(
			"Node %s references HTTP action rule %s, which is disabled in this project. Enable the rule under Settings → HTTP actions or select an enabled rule before saving.",
			quoted(nodeID), quoted(operation),
		),
	}
}

// HTTPNodeOperations extracts `(nodeID, operation)` for every top-level
// `type: http` node in instructions. Returns nil when the YAML has no HTTP nodes.
// YAML merge keys (`<<`) that supply or appear on an HTTP node are refused.
func HTTPNodeOperations(instructions string) ([]HTTPNodeOperation, error) {
	if len(instructions) > MaxInstructionsBytes {
		return nil, ErrInstructionsTooLarge
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil, nil
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	var ops []HTTPNodeOperation
	for i := 0; i+1 < len(root.Content); i += 2 {
		rootKey := aliased(root.Content[i])
		if rootKey.Tag == "!!merge" {
			if mergeIntroducesHTTPNodes(aliased(root.Content[i+1]), 0) {
				return nil, httpNodeRefusal(nil, "YAML merge keys are not allowed with HTTP Request nodes.")
			}
			continue
		}
		if rootKey.Value != "nodes" {
			continue
		}
		nodes := aliased(root.Content[i+1])
		if nodes.Kind != yaml.SequenceNode {
			continue
		}
		for _, raw := range nodes.Content {
			node := aliased(raw)
			if node.Kind != yaml.MappingNode {
				continue
			}
			var nodeID, nodeType, operation string
			var hasMerge, mergeHasHTTP bool
			for j := 0; j+1 < len(node.Content); j += 2 {
				keyNode := aliased(node.Content[j])
				v := aliased(node.Content[j+1])
				if keyNode.Tag == "!!merge" {
					hasMerge = true
					if mergeHasHTTPType(v, 0) {
						mergeHasHTTP = true
					}
					continue
				}
				switch keyNode.Value {
				case "id":
					if s, ok := graphIDScalar(v); ok {
						nodeID = s
					}
				case "type":
					nodeType, _ = stringScalar(v)
				case "operation":
					operation, _ = stringScalar(v)
				}
			}
			if (nodeType == "http" || mergeHasHTTP) && hasMerge {
				return nil, httpNodeRefusal(nil, "YAML merge keys are not allowed on HTTP Request nodes.")
			}
			if nodeType == "http" && operation != "" {
				ops = append(ops, HTTPNodeOperation{NodeID: nodeID, Operation: operation})
			}
		}
	}
	return ops, nil
}

func mergeHasHTTPType(node *yaml.Node, depth int) bool {
	if node == nil || depth > 8 {
		return false
	}
	node = aliased(node)
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := aliased(node.Content[i])
			v := aliased(node.Content[i+1])
			if k.Tag == "!!merge" && mergeHasHTTPType(v, depth+1) {
				return true
			}
			if k.Value == "type" {
				if t, ok := stringScalar(v); ok && t == "http" {
					return true
				}
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if mergeHasHTTPType(item, depth+1) {
				return true
			}
		}
	}
	return false
}

func mergeIntroducesHTTPNodes(node *yaml.Node, depth int) bool {
	if node == nil || depth > 8 {
		return false
	}
	node = aliased(node)
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := aliased(node.Content[i])
			v := aliased(node.Content[i+1])
			if k.Tag == "!!merge" && mergeIntroducesHTTPNodes(v, depth+1) {
				return true
			}
			if k.Value == "nodes" && v.Kind == yaml.SequenceNode {
				for _, entry := range v.Content {
					if mergeHasHTTPType(entry, depth+1) {
						return true
					}
				}
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if mergeIntroducesHTTPNodes(item, depth+1) {
				return true
			}
		}
	}
	return false
}

func checkStrictMappingKeys(id *yaml.Node, section string, m *yaml.Node) error {
	seen := make(map[string]bool, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := aliased(m.Content[i])
		if k.Tag == "!!merge" {
			return httpNodeRefusal(id, fmt.Sprintf("YAML merge keys are not allowed in %s.", section))
		}
		if seen[k.Value] {
			return httpNodeRefusal(id, fmt.Sprintf("duplicate field %q is not allowed in %s.", k.Value, section))
		}
		seen[k.Value] = true
	}
	return nil
}

func httpNodeRefusal(id *yaml.Node, reason string) error {
	return &apierr.APIError{
		Status:  http.StatusBadRequest,
		Code:    CodeHTTPNodeInvalid,
		Message: fmt.Sprintf("%s is not a valid HTTP Request node: %s", nodeLabel(id), reason),
	}
}

func checkHTTPNodes(rawNodes []*yaml.Node) error {
	for _, raw := range rawNodes {
		node := aliased(raw)
		if node.Kind != yaml.MappingNode {
			continue
		}
		all := make(map[string]*yaml.Node, len(node.Content)/2)
		var hasDup, hasMerge, mergeHasHTTP bool
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := aliased(node.Content[i])
			valNode := aliased(node.Content[i+1])
			if keyNode.Tag == "!!merge" {
				hasMerge = true
				if mergeHasHTTPType(valNode, 0) {
					mergeHasHTTP = true
				}
				continue
			}
			k := keyNode.Value
			if _, dup := all[k]; dup {
				hasDup = true
			}
			all[k] = valNode
		}
		nodeType, _ := stringScalar(all["type"])
		if nodeType != "http" && !mergeHasHTTP {
			continue
		}
		if hasMerge {
			return httpNodeRefusal(all["id"], "YAML merge keys are not allowed on HTTP Request nodes.")
		}
		if hasDup {
			return httpNodeRefusal(all["id"], "duplicate fields are not allowed.")
		}
		if err := checkOneHTTPNode(all); err != nil {
			return err
		}
	}
	return nil
}

func checkOneHTTPNode(fields map[string]*yaml.Node) error {
	id := fields["id"]
	for key := range fields {
		switch key {
		case "id", "type", "revision", "operation", "request", "response", "timeout_ms", "input", "output", "transition", settingsKey:
		case "url", "method", "credential", "configuration_id":
			return httpNodeRefusal(id, fmt.Sprintf("field %q is not allowed in pipeline YAML; URL, method, and credentials belong to the project HTTP action rule.", key))
		default:
			return httpNodeRefusal(id, fmt.Sprintf("unknown field %q is not allowed on an HTTP Request node.", key))
		}
	}
	rev, ok := optionalInt(fields["revision"])
	if !ok || rev == nil || *rev != 2 {
		return httpNodeRefusal(id, "revision must be 2.")
	}
	op, ok := stringScalar(fields["operation"])
	if !ok || len(op) == 0 || len(op) > httpMaxRuleNameBytes || !httpOperationPattern.MatchString(op) {
		return httpNodeRefusal(id, "operation must name a valid HTTP action rule (dot-separated lowercase identifiers, max 128 chars).")
	}
	if fields["request"] == nil || fields["request"].Kind != yaml.MappingNode {
		return httpNodeRefusal(id, "request must be a mapping of bindings.")
	}
	if err := checkHTTPRequest(id, fields["request"]); err != nil {
		return err
	}
	if fields["response"] == nil || fields["response"].Kind != yaml.MappingNode {
		return httpNodeRefusal(id, "response must be a mapping with a mode (json, text, or artifact).")
	}
	if err := checkHTTPResponse(id, fields["response"]); err != nil {
		return err
	}
	if rawTimeout, found := fields["timeout_ms"]; found {
		timeout, ok := optionalInt(rawTimeout)
		if !ok || timeout == nil || *timeout < 1 || *timeout > MaxHTTPTimeoutMs {
			return httpNodeRefusal(id, "timeout_ms must be an integer between 1 and 30000.")
		}
	}
	if rawInput, found := fields["input"]; found && present(rawInput) {
		if rawInput.Kind != yaml.SequenceNode || len(rawInput.Content) > httpMaxInputKeys {
			return httpNodeRefusal(id, "input must be a list of at most 64 state variable names.")
		}
		for _, item := range rawInput.Content {
			if name, ok := stringScalar(aliased(item)); !ok || name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
				return httpNodeRefusal(id, "each input entry must be a valid state variable name.")
			}
		}
	}
	rawOutput := fields["output"]
	if rawOutput == nil || rawOutput.Kind != yaml.SequenceNode || len(rawOutput.Content) != 1 {
		return httpNodeRefusal(id, "output must declare exactly one state variable.")
	}
	if outKey, ok := stringScalar(aliased(rawOutput.Content[0])); !ok || outKey == "" || len(outKey) > 256 || strings.ContainsAny(outKey, "\x00\r\n") {
		return httpNodeRefusal(id, "output must declare one valid state variable name.")
	}
	if rawTransition, found := fields["transition"]; found && present(rawTransition) {
		target, ok := graphIDScalar(rawTransition)
		if !ok || (target != "END" && !graphID.MatchString(target)) {
			return httpNodeRefusal(id, "transition must name END or a valid node identifier.")
		}
	}
	return nil
}

func checkHTTPRequest(id, req *yaml.Node) error {
	if err := checkStrictMappingKeys(id, "request", req); err != nil {
		return err
	}
	for i := 0; i+1 < len(req.Content); i += 2 {
		key := aliased(req.Content[i]).Value
		val := aliased(req.Content[i+1])
		switch key {
		case "path", "query":
			if err := checkHTTPBindingMap(id, key, val); err != nil {
				return err
			}
		case "headers":
			if err := checkHTTPHeaders(id, val); err != nil {
				return err
			}
		case "body":
			if err := checkHTTPBody(id, val); err != nil {
				return err
			}
		case "idempotency_key":
			if err := checkHTTPBinding(id, "request.idempotency_key", val, false, httpMaxLiteralBytes); err != nil {
				return err
			}
		default:
			return httpNodeRefusal(id, fmt.Sprintf("unknown field request.%s is not allowed.", key))
		}
	}
	return nil
}

func checkHTTPBindingMap(id *yaml.Node, section string, m *yaml.Node) error {
	if !present(m) {
		return nil
	}
	if m.Kind != yaml.MappingNode || len(m.Content)/2 > httpMaxBindingEntries {
		return httpNodeRefusal(id, fmt.Sprintf("request.%s must be a mapping of at most %d bindings.", section, httpMaxBindingEntries))
	}
	if err := checkStrictMappingKeys(id, "request."+section, m); err != nil {
		return err
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, ok := stringScalar(aliased(m.Content[i]))
		if !ok || !httpBindingKeyPattern.MatchString(key) {
			return httpNodeRefusal(id, fmt.Sprintf("request.%s contains an invalid binding name.", section))
		}
		if err := checkHTTPBinding(id, "request."+section, aliased(m.Content[i+1]), false, httpMaxLiteralBytes); err != nil {
			return err
		}
	}
	return nil
}

func checkHTTPHeaders(id, list *yaml.Node) error {
	if !present(list) {
		return nil
	}
	if list.Kind != yaml.SequenceNode || len(list.Content) > httpMaxHeaders {
		return httpNodeRefusal(id, fmt.Sprintf("request.headers must be a list of at most %d headers.", httpMaxHeaders))
	}
	seenHeaders := make(map[string]bool, len(list.Content))
	for _, entry := range list.Content {
		item := aliased(entry)
		if item.Kind != yaml.MappingNode {
			return httpNodeRefusal(id, "each request.headers entry must be a mapping.")
		}
		if err := checkStrictMappingKeys(id, "request.headers", item); err != nil {
			return err
		}
		var nameNode *yaml.Node
		for i := 0; i+1 < len(item.Content); i += 2 {
			if aliased(item.Content[i]).Value == "name" {
				nameNode = aliased(item.Content[i+1])
			}
		}
		name, ok := stringScalar(nameNode)
		if !ok || len(name) > httpMaxHeaderNameBytes || !httpHeaderNamePattern.MatchString(name) {
			return httpNodeRefusal(id, "each request.headers entry must carry a valid header name.")
		}
		lowerName := strings.ToLower(name)
		if seenHeaders[lowerName] {
			return httpNodeRefusal(id, fmt.Sprintf("duplicate header %s is not allowed.", quoted(name)))
		}
		seenHeaders[lowerName] = true
		if isRestrictedHTTPHeader(lowerName) {
			return httpNodeRefusal(id, fmt.Sprintf("header %s is restricted and cannot be set in pipeline YAML.", quoted(name)))
		}
		if err := checkHTTPBinding(id, "request.headers", item, true, httpMaxHeaderValueBytes); err != nil {
			return err
		}
	}
	return nil
}

func checkHTTPBinding(id *yaml.Node, section string, m *yaml.Node, allowName bool, maxLiteral int) error {
	if m.Kind != yaml.MappingNode {
		return httpNodeRefusal(id, fmt.Sprintf("%s entries must be mappings with either 'from' or 'value'.", section))
	}
	if err := checkStrictMappingKeys(id, section, m); err != nil {
		return err
	}
	var fromNode, valueNode, optNode *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := aliased(m.Content[i]).Value
		val := aliased(m.Content[i+1])
		switch key {
		case "from":
			fromNode = val
		case "value":
			valueNode = val
		case "optional":
			optNode = val
		case "name":
			if !allowName {
				return httpNodeRefusal(id, fmt.Sprintf("%s has unexpected field 'name'.", section))
			}
		default:
			return httpNodeRefusal(id, fmt.Sprintf("%s has unknown field %q.", section, key))
		}
	}
	switch {
	case fromNode != nil && valueNode == nil:
		stateKey, ok := stringScalar(fromNode)
		if !ok || !httpStateKeyPattern.MatchString(stateKey) {
			return httpNodeRefusal(id, fmt.Sprintf("%s 'from' must name a valid state key.", section))
		}
		if optNode != nil {
			if _, ok := boolScalar(optNode); !ok {
				return httpNodeRefusal(id, fmt.Sprintf("%s 'optional' must be a boolean.", section))
			}
		}
		return nil
	case valueNode != nil && fromNode == nil && optNode == nil:
		if valueNode.Kind != yaml.ScalarNode {
			return httpNodeRefusal(id, fmt.Sprintf("%s literal 'value' must be a string, integer, or boolean.", section))
		}
		if allowName {
			if valueNode.ShortTag() != "!!str" || len(valueNode.Value) > maxLiteral || !validHTTPHeaderValue(valueNode.Value) {
				return httpNodeRefusal(id, fmt.Sprintf("%s literal header 'value' must be a valid string within its byte limit.", section))
			}
			return nil
		}
		switch valueNode.ShortTag() {
		case "!!str":
			if len(valueNode.Value) > maxLiteral {
				return httpNodeRefusal(id, fmt.Sprintf("%s literal string value is invalid or exceeds its limit.", section))
			}
		case "!!bool":
			if _, ok := boolScalar(valueNode); !ok {
				return httpNodeRefusal(id, fmt.Sprintf("%s literal boolean value is invalid.", section))
			}
		case "!!int":
			if _, ok := optionalInt(valueNode); !ok {
				return httpNodeRefusal(id, fmt.Sprintf("%s literal integer value is invalid.", section))
			}
		default:
			return httpNodeRefusal(id, fmt.Sprintf("%s literal 'value' must be a string, integer, or boolean.", section))
		}
		return nil
	default:
		return httpNodeRefusal(id, fmt.Sprintf("%s must specify either 'from' or 'value' (never both, and 'optional' only with 'from').", section))
	}
}

func checkHTTPBody(id, body *yaml.Node) error {
	if !present(body) {
		return nil
	}
	if body.Kind != yaml.MappingNode {
		return httpNodeRefusal(id, "request.body must be a mapping.")
	}
	if err := checkStrictMappingKeys(id, "request.body", body); err != nil {
		return err
	}
	var kindNode, fromNode, valueNode, ctNode *yaml.Node
	for i := 0; i+1 < len(body.Content); i += 2 {
		key := aliased(body.Content[i]).Value
		val := aliased(body.Content[i+1])
		switch key {
		case "kind":
			kindNode = val
		case "from":
			fromNode = val
		case "value":
			valueNode = val
		case "content_type":
			ctNode = val
		default:
			return httpNodeRefusal(id, fmt.Sprintf("request.body has unknown field %q.", key))
		}
	}
	kind, ok := stringScalar(kindNode)
	if !ok {
		return httpNodeRefusal(id, "request.body.kind must be empty, json, text, or artifact.")
	}
	var contentType string
	if ctNode != nil {
		ct, ok := stringScalar(ctNode)
		if !ok || len(ct) > 128 || !httpMediaTypePattern.MatchString(ct) {
			return httpNodeRefusal(id, "request.body.content_type must be a valid media type.")
		}
		contentType = ct
	}
	if fromNode != nil {
		stateKey, ok := stringScalar(fromNode)
		if !ok || !httpStateKeyPattern.MatchString(stateKey) {
			return httpNodeRefusal(id, "request.body.from must name a valid state key.")
		}
	}
	switch kind {
	case "empty":
		if fromNode != nil || valueNode != nil || ctNode != nil {
			return httpNodeRefusal(id, "an empty request.body cannot carry from, value, or content_type.")
		}
	case "json":
		if contentType != "" {
			lower := strings.ToLower(contentType)
			if lower != "application/json" && !strings.HasSuffix(lower, "+json") {
				return httpNodeRefusal(id, "a JSON request.body content_type must be application/json or *+json.")
			}
		}
		if (fromNode != nil) == (valueNode != nil) {
			return httpNodeRefusal(id, "a json request.body must specify either 'from' or 'value'.")
		}
	case "text":
		if (fromNode != nil) == (valueNode != nil) {
			return httpNodeRefusal(id, "a text request.body must specify either 'from' or 'value'.")
		}
		if valueNode != nil {
			if _, ok := stringScalar(valueNode); !ok {
				return httpNodeRefusal(id, "a literal text request.body value must be a string.")
			}
		}
	case "artifact":
		if fromNode == nil || valueNode != nil || ctNode == nil {
			return httpNodeRefusal(id, "an artifact request.body requires 'from' and 'content_type'.")
		}
	default:
		return httpNodeRefusal(id, "request.body.kind must be empty, json, text, or artifact.")
	}
	return nil
}

func checkHTTPResponse(id, resp *yaml.Node) error {
	if err := checkStrictMappingKeys(id, "response", resp); err != nil {
		return err
	}
	var modeNode, statusesNode, maxBytesNode *yaml.Node
	for i := 0; i+1 < len(resp.Content); i += 2 {
		key := aliased(resp.Content[i]).Value
		val := aliased(resp.Content[i+1])
		switch key {
		case "mode":
			modeNode = val
		case "accepted_statuses":
			statusesNode = val
		case "max_bytes":
			maxBytesNode = val
		default:
			return httpNodeRefusal(id, fmt.Sprintf("response has unknown field %q.", key))
		}
	}
	mode, ok := stringScalar(modeNode)
	if !ok || (mode != "json" && mode != "text" && mode != "artifact") {
		return httpNodeRefusal(id, "response.mode must be json, text, or artifact.")
	}
	if maxBytesNode != nil {
		maxBytes, ok := optionalInt(maxBytesNode)
		if !ok || maxBytes == nil || *maxBytes < 1 || *maxBytes > httpMaxResponseBytes {
			return httpNodeRefusal(id, "response.max_bytes must be between 1 and 2097152.")
		}
	}
	if statusesNode != nil {
		if statusesNode.Kind != yaml.SequenceNode || len(statusesNode.Content) == 0 || len(statusesNode.Content) > httpMaxAcceptedStatuses {
			return httpNodeRefusal(id, "response.accepted_statuses must have 1 to 16 status ranges.")
		}
		for _, entry := range statusesNode.Content {
			item := aliased(entry)
			if item.Kind != yaml.MappingNode {
				return httpNodeRefusal(id, "each response.accepted_statuses entry must be a {first, last} mapping.")
			}
			if err := checkStrictMappingKeys(id, "response.accepted_statuses", item); err != nil {
				return err
			}
			var first, last *int64
			for i := 0; i+1 < len(item.Content); i += 2 {
				key := aliased(item.Content[i]).Value
				val := aliased(item.Content[i+1])
				switch key {
				case "first":
					n, ok := optionalInt(val)
					if !ok || n == nil {
						return httpNodeRefusal(id, "response.accepted_statuses.first must be an integer.")
					}
					first = n
				case "last":
					n, ok := optionalInt(val)
					if !ok || n == nil {
						return httpNodeRefusal(id, "response.accepted_statuses.last must be an integer.")
					}
					last = n
				default:
					return httpNodeRefusal(id, fmt.Sprintf("response.accepted_statuses has unknown field %q.", key))
				}
			}
			if first == nil || last == nil || *first < 200 || *last > 599 || *first > *last ||
				(*first <= 399 && *last >= 300) || (*first <= 401 && *last >= 401) || (*first <= 403 && *last >= 403) {
				return httpNodeRefusal(id, "response.accepted_statuses must lie within 200-599 and cannot include 3xx, 401, or 403.")
			}
		}
	}
	return nil
}

func validHTTPHeaderValue(value string) bool {
	if strings.ContainsAny(value, "\x00\r\n") || !httpguts.ValidHeaderFieldValue(value) {
		return false
	}
	for _, r := range value {
		if r == '\u0085' || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func isRestrictedHTTPHeader(lower string) bool {
	if strings.HasPrefix(lower, "x-elitea-") || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "x-auth-") {
		return true
	}
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key",
		"host", "connection", "content-length", "content-type", "transfer-encoding", "te",
		"trailer", "upgrade", "proxy-connection", "keep-alive", "idempotency-key", "forwarded":
		return true
	}
	return false
}
