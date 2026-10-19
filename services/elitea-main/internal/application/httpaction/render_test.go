package httpaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const renderVectorsPath = "../../../../../libs/jsonschema/runtime/v1/fixtures/http-action-render-vectors-v1.json"

type renderVector struct {
	ID     string          `json:"id"`
	Rule   json.RawMessage `json:"rule"`
	Node   json.RawMessage `json:"node"`
	Inputs json.RawMessage `json:"inputs"`
	Expect struct {
		Error string `json:"error"`
		OK    *struct {
			Method        string   `json:"method"`
			Path          string   `json:"path"`
			Query         string   `json:"query"`
			URL           string   `json:"url"`
			Headers       []Header `json:"headers"`
			BodyB64       string   `json:"body_b64"`
			BodySHA256    string   `json:"body_sha256"`
			BodyUTF8      string   `json:"body_utf8"`
			RequestWire   string   `json:"request_wire"`
			RequestDigest string   `json:"request_digest"`
		} `json:"ok"`
	} `json:"expect"`
}

func loadRenderVectors(t *testing.T) []renderVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(renderVectorsPath))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []renderVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) < 25 {
		t.Fatalf("expected at least 25 render vectors, got %d", len(file.Vectors))
	}
	return file.Vectors
}

func vectorRule(t *testing.T, raw json.RawMessage) Rule {
	t.Helper()
	var rule Rule
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rule); err != nil {
		t.Fatal(err)
	}
	rule.Normalize()
	return rule
}

// TestRenderVectors is the Go half of the cross-language render contract
// (http-action-render-vectors-v1.json); the Rust half is render_vectors_match.
func TestRenderVectors(t *testing.T) {
	for _, vector := range loadRenderVectors(t) {
		t.Run(vector.ID, func(t *testing.T) {
			rule := vectorRule(t, vector.Rule)
			node, err := ParseNode(vector.Node)
			if err != nil {
				t.Fatalf("node: %v", err)
			}
			inputs, err := CanonicalJSON(vector.Inputs)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := Render(rule, node.Template, inputs)
			if vector.Expect.Error != "" {
				if code := FailureCode(err); code != vector.Expect.Error {
					t.Fatalf("want error %q, got %v (%q)", vector.Expect.Error, err, code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ok := vector.Expect.OK
			if rendered.Request.Method != ok.Method || rendered.Path != ok.Path || rendered.Query != ok.Query || rendered.Request.URL != ok.URL {
				t.Fatalf("method/path/query/url: %s %s %s %s", rendered.Request.Method, rendered.Path, rendered.Query, rendered.Request.URL)
			}
			if string(rendered.Wire) != ok.RequestWire {
				t.Fatalf("wire\n got %s\nwant %s", rendered.Wire, ok.RequestWire)
			}
			if rendered.Digest != ok.RequestDigest {
				t.Fatalf("digest %s", rendered.Digest)
			}
			headers := rendered.Headers
			if headers == nil {
				headers = []Header{}
			}
			want, _ := json.Marshal(ok.Headers)
			got, _ := json.Marshal(headers)
			if !bytes.Equal(want, got) {
				t.Fatalf("headers %s want %s", got, want)
			}
			sum := sha256.Sum256(rendered.Body)
			if base64.StdEncoding.EncodeToString(rendered.Body) != ok.BodyB64 || hex.EncodeToString(sum[:]) != ok.BodySHA256 || string(rendered.Body) != ok.BodyUTF8 {
				t.Fatalf("body %q", rendered.Body)
			}
		})
	}
}

func basicRule() Rule {
	rule := Rule{Name: "tracker.create_item", Origin: "https://api.example.com", PathTemplate: "/v1/items/{item}", Method: "POST", QueryNames: []string{"q"}, ConfigurationID: 7, Toolkit: "tracker", Tool: "create_item", Enabled: true}
	rule.Normalize()
	return rule
}

func basicTemplate(t *testing.T, request string) NodeTemplate {
	t.Helper()
	node, err := ParseNode([]byte(`{"id":"n","type":"http","revision":2,"operation":"tracker.create_item","request":` + request + `,"response":{"mode":"json"},"output":["out"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return node.Template
}

func TestRenderInputsBoundary(t *testing.T) {
	template := basicTemplate(t, `{"path":{"item":{"from":"k"}},"body":{"kind":"text","from":"b","content_type":"text/plain"}}`)
	build := func(total int) []byte {
		prefix := `{"body":"`
		suffix := `","path":{"item":"i"}}`
		return []byte(prefix + strings.Repeat("a", total-len(prefix)-len(suffix)) + suffix)
	}
	at := build(MaxInputsBytes)
	if len(at) != MaxInputsBytes {
		t.Fatal(len(at))
	}
	if _, err := DecodeInputs(at); err != nil {
		t.Fatalf("256 KiB inputs refused: %v", err)
	}
	// The text body itself stays within the 256 KiB body bound of Validate.
	if _, err := Render(basicRule(), template, build(200*1024)); err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := DecodeInputs(build(MaxInputsBytes + 1)); FailureCode(err) != CodeInputsTooBig || !errors.Is(err, ErrInvalid) {
		t.Fatalf("256 KiB + 1 not refused as inputs_too_large: %v", err)
	}
}

func TestRenderRefusesNonCanonicalOrUnknownInputs(t *testing.T) {
	template := basicTemplate(t, `{"path":{"item":{"from":"k"}}}`)
	for name, inputs := range map[string]string{
		"spaces":         `{"path": {"item":"i"}}`,
		"unsorted":       `{"path":{"item":"i"},"body":1}`,
		"unknown key":    `{"extra":1,"path":{"item":"i"}}`,
		"extra path":     `{"path":{"item":"i","other":"x"}}`,
		"duplicate":      `{"path":{"item":"i","item":"j"}}`,
		"escaped u2028":  `{"path":{"item":"` + "\\u2028" + `"}}`,
		"trailing":       `{"path":{"item":"i"}}` + "\n",
		"body unbound":   `{"body":1,"path":{"item":"i"}}`,
		"key unbound":    `{"idempotency_key":"k","path":{"item":"i"}}`,
		"not an object":  `[]`,
		"float in path":  `{"path":{"item":1.5}}`,
		"null in path":   `{"path":{"item":null}}`,
		"object in path": `{"path":{"item":{}}}`,
	} {
		if _, err := Render(basicRule(), template, []byte(inputs)); FailureCode(err) != CodeInvalidInput {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRenderSegmentBoundary(t *testing.T) {
	template := basicTemplate(t, `{"path":{"item":{"from":"k"}}}`)
	inputs := func(value string) []byte {
		raw, _ := CanonicalJSON([]byte(`{"path":{"item":` + mustJSON(value) + `}}`))
		return raw
	}
	if _, err := Render(basicRule(), template, inputs(strings.Repeat("a", MaxSegmentBytes))); err != nil {
		t.Fatalf("1024-byte segment refused: %v", err)
	}
	for _, value := range []string{strings.Repeat("a", MaxSegmentBytes+1), "", ".", ".."} {
		if _, err := Render(basicRule(), template, inputs(value)); FailureCode(err) != CodeInvalidInput {
			t.Fatalf("segment %d bytes: %v", len(value), err)
		}
	}
}

// Encoding (gate §3a): separators and control characters in path, query and
// header values cannot escape the operation or the base URL.
func TestRenderEncodingCannotEscapeOperation(t *testing.T) {
	template := basicTemplate(t, `{"path":{"item":{"from":"k"}},"query":{"q":{"from":"q"}}}`)
	for _, hostile := range []string{"../admin", "..%2F..%2Fadmin", "a/b", "%2F", "a\r\nX-Injected: 1", "a\x00b", "a b", "a b", "a\u0085b", "a?b#c", "@evil.example", "//evil.example", "\\evil", "a b"} {
		inputs, err := CanonicalJSON([]byte(`{"path":{"item":` + mustJSON(hostile) + `},"query":{"q":` + mustJSON(hostile) + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := Render(basicRule(), template, inputs)
		if err != nil {
			t.Fatalf("%q: %v", hostile, err)
		}
		const prefix = "https://api.example.com/v1/items/"
		if !strings.HasPrefix(rendered.Request.URL, prefix) {
			t.Fatalf("%q escaped the origin: %s", hostile, rendered.Request.URL)
		}
		segment, query, found := strings.Cut(strings.TrimPrefix(rendered.Request.URL, prefix), "?q=")
		if !found || strings.ContainsAny(segment, "/?#\\@ \r\n\x00") || strings.ContainsAny(query, "&=/?#\\ \r\n\x00") {
			t.Fatalf("%q was not encoded into one segment and one pair: %s", hostile, rendered.Request.URL)
		}
		for _, c := range segment + query {
			if c > 0x7e {
				t.Fatalf("%q left a raw non-ASCII byte: %s", hostile, rendered.Request.URL)
			}
		}
	}
	headers := basicTemplate(t, `{"path":{"item":{"value":"i"}},"headers":[{"name":"x-a","from":"h"}]}`)
	for _, hostile := range []string{"a\r\nb: c", "a\nb", "a\rb", "a\x00b"} {
		inputs, _ := CanonicalJSON([]byte(`{"headers":{"x-a":` + mustJSON(hostile) + `}}`))
		if _, err := Render(basicRule(), headers, inputs); FailureCode(err) != CodeInvalidInput {
			t.Fatalf("header %q: %v", hostile, err)
		}
	}
}

func TestRenderRefusesEveryRestrictedHeader(t *testing.T) {
	for _, name := range []string{"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "host", "connection", "content-length", "content-type", "transfer-encoding", "te", "trailer", "upgrade", "proxy-connection", "keep-alive", "idempotency-key", "x-elitea-claim-id"} {
		template := basicTemplate(t, `{"path":{"item":{"value":"i"}},"headers":[{"name":"`+name+`","value":"v"}]}`)
		if _, err := Render(basicRule(), template, []byte(`{}`)); FailureCode(err) != CodeInvalidInput {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRenderPolicyDeniedForShapeMismatch(t *testing.T) {
	for name, request := range map[string]string{
		"missing placeholder": `{"path":{}}`,
		"extra placeholder":   `{"path":{"item":{"value":"i"},"other":{"value":"x"}}}`,
		"query not allowed":   `{"path":{"item":{"value":"i"}},"query":{"debug":{"value":"1"}}}`,
	} {
		if _, err := Render(basicRule(), basicTemplate(t, request), []byte(`{}`)); FailureCode(err) != CodePolicyDenied || !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseNodeRefusesURLMethodCredentialAndUnknownFields(t *testing.T) {
	base := `{"id":"n","type":"http","revision":2,"operation":"a.b","request":{},"response":{"mode":"json"},"output":["o"]`
	for name, raw := range map[string]string{
		"url":             base + `,"url":"https://x"}`,
		"method":          base + `,"method":"GET"}`,
		"credential":      `{"id":"n","type":"http","revision":2,"operation":"a.b","request":{"credential":{"configuration_id":1}},"response":{"mode":"json"},"output":["o"]}`,
		"revision 1":      strings.Replace(base, `"revision":2`, `"revision":1`, 1) + `}`,
		"timeout 30001":   base + `,"timeout_ms":30001}`,
		"timeout 0":       base + `,"timeout_ms":0}`,
		"two outputs":     strings.Replace(base, `["o"]`, `["o","p"]`, 1) + `}`,
		"from and value":  `{"id":"n","type":"http","revision":2,"operation":"a.b","request":{"path":{"a":{"from":"x","value":"y"}}},"response":{"mode":"json"},"output":["o"]}`,
		"float literal":   `{"id":"n","type":"http","revision":2,"operation":"a.b","request":{"path":{"a":{"value":1.5}}},"response":{"mode":"json"},"output":["o"]}`,
		"3xx accepted":    strings.Replace(base, `{"mode":"json"}`, `{"mode":"json","accepted_statuses":[{"first":200,"last":302}]}`, 1) + `}`,
		"max_bytes 2MiB+": strings.Replace(base, `{"mode":"json"}`, `{"mode":"json","max_bytes":2097153}`, 1) + `}`,
	} {
		if _, err := ParseNode([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseNode([]byte(base + `,"timeout_ms":30000}`)); err != nil {
		t.Fatalf("timeout 30000 refused: %v", err)
	}
}

func mustJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
