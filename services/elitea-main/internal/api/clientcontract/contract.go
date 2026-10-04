// Package clientcontract is the additive-only gate over the `client` subset of
// api/openapi/v2.yaml (ADR-0025 decision 6).
//
// The operations a native client depends on are tagged `client`. Within one
// client_contract major version that subset may only grow: a client released
// against version 1.x must keep working against every later 1.y server, and a
// native client cannot be upgraded in step with a deployment.
//
// The gate works over a NORMALIZED surface rather than the YAML text, so that a
// reworded description, a moved schema or a `$ref` that is inlined does not
// count as a change, while a removed field does:
//
//   - Normalize reduces every client operation to what a client can observe —
//     parameters, request body and responses, with every `$ref` resolved and
//     every `allOf` merged — keyed "METHOD /path".
//   - Compare reads a committed lock (api/openapi/client-contract/v<N>.lock.json)
//     as the promise and reports every way the current spec breaks it.
//
// Direction matters, so the rules are not symmetric. The REQUEST side is what a
// client sends: the server may stop requiring something but may not start. The
// RESPONSE side is what a client reads: the server may add but may not remove,
// and may not make a guaranteed field optional or nullable.
//
// The lock is committed, not computed from a git base, so the gate gives the
// same answer locally, in a merge queue and on a shallow clone — the reasoning
// of apps/elitea-web/scripts/contract-coverage.lock.json.
package clientcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Tag is the tag that puts an operation in the client contract.
const Tag = "client"

// VersionExtension is the `info` extension that states the contract version
// the document speaks, "MAJOR.MINOR".
const VersionExtension = "x-elitea-client-contract"

// Surface is the normalized client subset of one OpenAPI document. It is also
// the lock file's format.
type Surface struct {
	// ClientContract is the document's `info.x-elitea-client-contract`.
	ClientContract string `json:"client_contract"`
	// Operations is keyed "METHOD /path", the path as written in the document.
	Operations map[string]*Operation `json:"operations"`
}

// Operation is what a client can observe of one operation.
type Operation struct {
	OperationID string `json:"operation_id"`
	// Parameters is keyed "in:name" (header names lower-cased).
	Parameters  map[string]*Parameter `json:"parameters,omitempty"`
	RequestBody *Body                 `json:"request_body,omitempty"`
	// Responses is keyed by status code ("200", "4XX", "default").
	Responses map[string]*Response `json:"responses"`
}

// Parameter is one path, query or header parameter.
type Parameter struct {
	Required bool   `json:"required,omitempty"`
	Schema   *Shape `json:"schema,omitempty"`
}

// Body is a request body; Content is keyed by media type.
type Body struct {
	Required bool              `json:"required,omitempty"`
	Content  map[string]*Shape `json:"content"`
}

// Response is one response; Content is keyed by media type and Headers by the
// lower-cased header name.
type Response struct {
	Content map[string]*Shape `json:"content,omitempty"`
	Headers map[string]*Shape `json:"headers,omitempty"`
}

// Shape is a schema reduced to what decides compatibility.
type Shape struct {
	Type     string   `json:"type,omitempty"`
	Format   string   `json:"format,omitempty"`
	Nullable bool     `json:"nullable,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	// Required lists the object properties that must be present.
	Required   []string          `json:"required,omitempty"`
	Properties map[string]*Shape `json:"properties,omitempty"`
	// Open is true for an object that admits keys it does not name.
	Open  bool   `json:"open,omitempty"`
	Items *Shape `json:"items,omitempty"`
	// OneOf holds the alternatives of a oneOf/anyOf, sorted by their JSON.
	OneOf []*Shape `json:"one_of,omitempty"`
	// Ref names a component schema already being expanded higher up — a
	// recursive schema is cut there rather than expanded for ever.
	Ref string `json:"ref,omitempty"`
}

// Normalize returns the client surface of doc. It fails when an operation is
// tagged `client` other than LAST: the web client's generator files an
// operation under its first tag, so `client` first would move every generated
// hook of that operation into another folder.
func Normalize(doc *openapi3.T) (*Surface, error) {
	if doc == nil || doc.Paths == nil {
		return nil, fmt.Errorf("clientcontract: document has no paths")
	}
	surface := &Surface{Operations: map[string]*Operation{}}
	if doc.Info != nil {
		if raw, ok := doc.Info.Extensions[VersionExtension]; ok {
			version, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("clientcontract: info.%s must be a string, got %T", VersionExtension, raw)
			}
			surface.ClientContract = version
		}
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op == nil || !hasTag(op.Tags, Tag) {
				continue
			}
			if op.Tags[len(op.Tags)-1] != Tag {
				return nil, fmt.Errorf("clientcontract: %s %s (%s) tags %v: `%s` must be the LAST tag", strings.ToUpper(method), path, op.OperationID, op.Tags, Tag)
			}
			key := strings.ToUpper(method) + " " + path
			surface.Operations[key] = normalizeOperation(item, op)
		}
	}
	return surface, nil
}

func hasTag(tags []string, tag string) bool {
	for _, candidate := range tags {
		if candidate == tag {
			return true
		}
	}
	return false
}

func normalizeOperation(item *openapi3.PathItem, op *openapi3.Operation) *Operation {
	out := &Operation{OperationID: op.OperationID, Responses: map[string]*Response{}}
	params := map[string]*Parameter{}
	add := func(refs openapi3.Parameters) {
		for _, ref := range refs {
			if ref == nil || ref.Value == nil {
				continue
			}
			p := ref.Value
			name := p.Name
			if p.In == openapi3.ParameterInHeader {
				name = strings.ToLower(name)
			}
			params[p.In+":"+name] = &Parameter{Required: p.Required, Schema: newWalker().shape(p.Schema)}
		}
	}
	// Operation-level parameters override path-level ones of the same in+name.
	add(item.Parameters)
	add(op.Parameters)
	if len(params) > 0 {
		out.Parameters = params
	}
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		body := &Body{Required: op.RequestBody.Value.Required, Content: map[string]*Shape{}}
		for media, content := range op.RequestBody.Value.Content {
			body.Content[media] = newWalker().shape(content.Schema)
		}
		out.RequestBody = body
	}
	if op.Responses != nil {
		for status, ref := range op.Responses.Map() {
			if ref == nil || ref.Value == nil {
				continue
			}
			response := &Response{}
			if len(ref.Value.Content) > 0 {
				response.Content = map[string]*Shape{}
				for media, content := range ref.Value.Content {
					response.Content[media] = newWalker().shape(content.Schema)
				}
			}
			if len(ref.Value.Headers) > 0 {
				response.Headers = map[string]*Shape{}
				for name, header := range ref.Value.Headers {
					if header == nil || header.Value == nil {
						continue
					}
					response.Headers[strings.ToLower(name)] = newWalker().shape(header.Value.Schema)
				}
			}
			out.Responses[status] = response
		}
	}
	return out
}

// walker expands one schema graph. `active` holds the component refs on the
// current descent path, which is what cuts a recursive schema.
type walker struct {
	active map[string]bool
}

func newWalker() *walker { return &walker{active: map[string]bool{}} }

func (w *walker) shape(ref *openapi3.SchemaRef) *Shape {
	if ref == nil || ref.Value == nil {
		return &Shape{}
	}
	if ref.Ref != "" {
		if w.active[ref.Ref] {
			return &Shape{Ref: componentName(ref.Ref)}
		}
		w.active[ref.Ref] = true
		defer delete(w.active, ref.Ref)
	}
	s := ref.Value
	out := &Shape{Format: s.Format, Nullable: s.Nullable}
	if s.Type != nil && len(*s.Type) > 0 {
		types := append([]string(nil), (*s.Type)...)
		sort.Strings(types)
		out.Type = strings.Join(types, "|")
	}
	for _, value := range s.Enum {
		out.Enum = append(out.Enum, fmt.Sprint(value))
	}
	sort.Strings(out.Enum)

	required := map[string]bool{}
	for _, name := range s.Required {
		required[name] = true
	}
	if len(s.Properties) > 0 {
		out.Properties = map[string]*Shape{}
		for name, prop := range s.Properties {
			out.Properties[name] = w.shape(prop)
		}
	}
	if s.AdditionalProperties.Has != nil && *s.AdditionalProperties.Has {
		out.Open = true
	}
	if s.AdditionalProperties.Schema != nil {
		out.Open = true
	}
	if s.Items != nil {
		out.Items = w.shape(s.Items)
	}
	// allOf is a conjunction: every branch's properties and requirements
	// apply. Merging them makes `allOf: [$ref X]` and X itself the same shape,
	// so wrapping a schema to add `nullable` is not a change.
	for _, branch := range s.AllOf {
		merged := w.shape(branch)
		if out.Type == "" {
			out.Type = merged.Type
		}
		if out.Format == "" {
			out.Format = merged.Format
		}
		out.Nullable = out.Nullable || merged.Nullable
		out.Open = out.Open || merged.Open
		if out.Items == nil {
			out.Items = merged.Items
		}
		if len(out.Enum) == 0 {
			out.Enum = merged.Enum
		}
		if out.Ref == "" && merged.Ref != "" && len(merged.Properties) == 0 {
			out.Ref = merged.Ref
		}
		for name, prop := range merged.Properties {
			if out.Properties == nil {
				out.Properties = map[string]*Shape{}
			}
			if _, exists := out.Properties[name]; !exists {
				out.Properties[name] = prop
			}
		}
		for _, name := range merged.Required {
			required[name] = true
		}
		out.OneOf = append(out.OneOf, merged.OneOf...)
	}
	for _, branches := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf} {
		for _, branch := range branches {
			out.OneOf = append(out.OneOf, w.shape(branch))
		}
	}
	sort.Slice(out.OneOf, func(i, j int) bool { return canonical(out.OneOf[i]) < canonical(out.OneOf[j]) })
	for name := range required {
		out.Required = append(out.Required, name)
	}
	sort.Strings(out.Required)
	if out.Type == "" && len(out.Properties) > 0 {
		out.Type = "object"
	}
	return out
}

func componentName(ref string) string {
	return ref[strings.LastIndex(ref, "/")+1:]
}

func canonical(s *Shape) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// Marshal renders a surface as the lock file's bytes: indented, with sorted
// keys (encoding/json sorts map keys), and a trailing newline.
func Marshal(surface *Surface) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(surface); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unmarshal reads a lock file.
func Unmarshal(data []byte) (*Surface, error) {
	var surface Surface
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&surface); err != nil {
		return nil, err
	}
	if len(surface.Operations) == 0 {
		return nil, fmt.Errorf("clientcontract: lock has no operations")
	}
	return &surface, nil
}

// Major returns the major part of a "MAJOR.MINOR" version.
func Major(version string) (string, error) {
	major, minor, ok := strings.Cut(version, ".")
	if !ok || major == "" || minor == "" || strings.Trim(major, "0123456789") != "" || strings.Trim(minor, "0123456789") != "" {
		return "", fmt.Errorf("clientcontract: version %q is not MAJOR.MINOR", version)
	}
	return major, nil
}
