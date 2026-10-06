// Package httpaction owns the admitted HTTP action and effect receipt contract.
package httpaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Schema = "elitea.runtime.http-action.v2"
const ReceiptSchema = "elitea.runtime.http-action-receipt.v2"
const MaxRequest = 384 * 1024
const MaxInvocation = 768 * 1024
const MaxResponse = 2 * 1024 * 1024
const MaxInline = 512 * 1024

var ErrInvalid = errors.New("HTTP action input is invalid")
var ErrUnauthorized = errors.New("HTTP action authority is unavailable")
var ErrUnavailable = errors.New("HTTP action dependency is unavailable")

type Invocation struct {
	SchemaVersion  string          `json:"schema_version"`
	NodeID         string          `json:"node_id"`
	ThreadID       string          `json:"thread_id"`
	Step           uint64          `json:"step"`
	ActivationID   string          `json:"activation_id"`
	RequestDigest  string          `json:"request_digest"`
	BindingDigest  string          `json:"binding_digest"`
	RequestWireB64 string          `json:"request_wire_b64"`
	Request        json.RawMessage `json:"-"`
}
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type CredentialReference struct {
	ConfigurationID int32 `json:"configuration_id"`
}
type Artifact struct {
	Reference        string `json:"reference"`
	ImmutableVersion string `json:"immutable_version"`
	SHA256           string `json:"sha256"`
	ByteLength       uint64 `json:"byte_length"`
}
type Body struct {
	Kind        string          `json:"kind"`
	Value       json.RawMessage `json:"value,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Reference   *Artifact       `json:"reference,omitempty"`
}
type StatusRange struct {
	First uint16 `json:"first"`
	Last  uint16 `json:"last"`
}
type ResponseContract struct {
	Mode             string        `json:"mode"`
	AcceptedStatuses []StatusRange `json:"accepted_statuses,omitempty"`
	MaxBytes         uint64        `json:"max_bytes,omitempty"`
}
type Request struct {
	Method         string               `json:"method"`
	URL            string               `json:"url"`
	Headers        []Header             `json:"headers,omitempty"`
	Credential     *CredentialReference `json:"credential,omitempty"`
	Body           Body                 `json:"body,omitempty"`
	Response       ResponseContract     `json:"response"`
	TimeoutMS      uint32               `json:"timeout_ms,omitempty"`
	IdempotencyKey string               `json:"idempotency_key,omitempty"`
}
type Projection struct {
	Status      uint16  `json:"status"`
	ContentType *string `json:"content_type"`
	ByteLength  uint64  `json:"byte_length"`
	Data        Data    `json:"data"`
}
type Data struct {
	Kind      string    `json:"kind"`
	Value     any       `json:"value,omitempty"`
	Reference *Artifact `json:"reference,omitempty"`
}
type Receipt struct {
	SchemaVersion string      `json:"schema_version"`
	ActivationID  string      `json:"activation_id"`
	RequestDigest string      `json:"request_digest"`
	BindingDigest string      `json:"binding_digest"`
	EffectID      string      `json:"effect_id"`
	State         string      `json:"state"`
	FailureCode   *string     `json:"failure_code"`
	Result        *Projection `json:"result"`
}

func Decode(data []byte, target any) error {
	limit := MaxRequest
	if _, envelope := target.(*Invocation); envelope {
		limit = MaxInvocation
	}
	if len(data) == 0 || len(data) > limit || !strictJSON(data, 40, 20000) {
		return ErrInvalid
	}
	var required []string
	switch target.(type) {
	case *Invocation:
		required = []string{"schema_version", "node_id", "thread_id", "step", "activation_id", "request_digest", "binding_digest", "request_wire_b64"}
	case *Request:
		required = []string{"method", "url", "response"}
	}
	if len(required) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return ErrInvalid
		}
		for _, name := range required {
			value, found := fields[name]
			if !found || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return ErrInvalid
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func (inv Invocation) RequestBytes() ([]byte, error) {
	if len(inv.RequestWireB64) > base64.StdEncoding.EncodedLen(MaxRequest) {
		return nil, ErrInvalid
	}
	wire, err := base64.StdEncoding.Strict().DecodeString(inv.RequestWireB64)
	if err != nil || len(wire) == 0 || len(wire) > MaxRequest || base64.StdEncoding.EncodeToString(wire) != inv.RequestWireB64 || inv.Request != nil && !bytes.Equal(inv.Request, wire) {
		return nil, ErrInvalid
	}
	return wire, nil
}
func BindingBytes(value string) [32]byte {
	var result [32]byte
	raw, _ := hex.DecodeString(value)
	copy(result[:], raw)
	return result
}
func Parse(inv Invocation) (Request, error) {
	wire, err := inv.RequestBytes()
	if err != nil {
		return Request{}, err
	}
	inv.Request = wire

	if inv.SchemaVersion != Schema || inv.NodeID == "" || len(inv.NodeID) > 128 || inv.ThreadID == "" || len(inv.ThreadID) > 512 || !ValidDigest(inv.RequestDigest) || !ValidDigest(inv.ActivationID) || !ValidDigest(inv.BindingDigest) {
		return Request{}, ErrInvalid
	}
	digest := sha256.Sum256(inv.Request)
	if hex.EncodeToString(digest[:]) != inv.RequestDigest || VisitID(inv.ThreadID, inv.NodeID, inv.Step, BindingBytes(inv.BindingDigest)) != inv.ActivationID {
		return Request{}, ErrInvalid
	}
	var request Request
	if Decode(inv.Request, &request) != nil {
		return Request{}, ErrInvalid
	}
	var present map[string]json.RawMessage
	if json.Unmarshal(inv.Request, &present) != nil {
		return Request{}, ErrInvalid
	}
	if value, exists := present["timeout_ms"]; exists && (string(value) == "0" || string(value) == "null") {
		return Request{}, ErrInvalid
	}
	var responsePresent map[string]json.RawMessage
	if json.Unmarshal(present["response"], &responsePresent) != nil {
		return Request{}, ErrInvalid
	}
	if value, exists := responsePresent["max_bytes"]; exists && (string(value) == "0" || string(value) == "null") {
		return Request{}, ErrInvalid
	}
	if value, exists := present["idempotency_key"]; exists && string(value) == `""` {
		return Request{}, ErrInvalid
	}
	if value, exists := present["body"]; exists {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || fields == nil || len(fields["kind"]) == 0 || request.Body.Kind == "" {
			return Request{}, ErrInvalid
		}
	}
	if value, exists := present["headers"]; exists {
		var headers []map[string]json.RawMessage
		if json.Unmarshal(value, &headers) != nil || headers == nil {
			return Request{}, ErrInvalid
		}
		for _, header := range headers {
			if len(header["name"]) == 0 || len(header["value"]) == 0 || string(header["value"]) == "null" {
				return Request{}, ErrInvalid
			}
		}
	}
	if value, exists := responsePresent["accepted_statuses"]; exists && string(value) == "null" {
		return Request{}, ErrInvalid
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}
func VisitID(thread, node string, step uint64, request [32]byte) string {
	h := sha256.New()
	h.Write([]byte("elitea.graph.http.activation.v1\x00"))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], step)
	for _, part := range [][]byte{[]byte(thread), []byte(node), number[:], request[:]} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func EffectID(execution string, generation uint64, inv Invocation) string {
	h := sha256.New()
	h.Write([]byte("elitea.http.effect.v2\x00"))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], generation)
	for _, part := range [][]byte{[]byte(execution), number[:], []byte(inv.ActivationID), []byte(inv.RequestDigest), []byte(inv.BindingDigest)} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func ValidDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func (r *Request) Validate() error {
	switch r.Method {
	case "GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE":
	default:
		return ErrInvalid
	}
	u, err := url.Parse(r.URL)
	if err != nil || len(r.URL) > 8192 || strings.TrimSpace(r.URL) != r.URL || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(r.URL, "\\{}") || strings.IndexFunc(r.URL, unicode.IsControl) >= 0 {
		return ErrInvalid
	}
	for name := range u.Query() {
		switch strings.ToLower(name) {
		case "access_token", "token", "api_key", "apikey", "password", "client_secret", "authorization":
			return ErrInvalid
		}
	}
	if r.TimeoutMS == 0 {
		r.TimeoutMS = 30000
	}
	if r.TimeoutMS > 30000 {
		return ErrInvalid
	}
	if r.Credential != nil && r.Credential.ConfigurationID <= 0 {
		return ErrInvalid
	}
	if len(r.Headers) > 32 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	total := 0
	for _, header := range r.Headers {
		name := strings.ToLower(header.Name)
		total += len(header.Name) + len(header.Value)
		if RestrictedHeader(name) || !validHeaderName(name) || seen[name] || len(header.Name) > 128 || len(header.Value) > 8192 || total > 16384 || strings.ContainsAny(header.Value, "\r\n\x00") {
			return ErrInvalid
		}
		seen[name] = true
	}
	if r.IdempotencyKey != "" {
		if len(r.IdempotencyKey) > 128 {
			return ErrInvalid
		}
		for _, c := range []byte(r.IdempotencyKey) {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._:-", rune(c)) {
				return ErrInvalid
			}
		}
	}
	if r.Response.MaxBytes == 0 {
		r.Response.MaxBytes = MaxResponse
	}
	if r.Response.MaxBytes > MaxResponse {
		return ErrInvalid
	}
	if r.Response.Mode != "json" && r.Response.Mode != "text" && r.Response.Mode != "artifact" {
		return ErrInvalid
	}
	if r.Response.AcceptedStatuses == nil {
		r.Response.AcceptedStatuses = []StatusRange{{200, 299}}
	}
	if len(r.Response.AcceptedStatuses) == 0 || len(r.Response.AcceptedStatuses) > 16 {
		return ErrInvalid
	}
	for _, v := range r.Response.AcceptedStatuses {
		if v.First < 200 || v.Last > 599 || v.First > v.Last || v.First <= 399 && v.Last >= 300 || v.First <= 401 && v.Last >= 401 || v.First <= 403 && v.Last >= 403 {
			return ErrInvalid
		}
	}
	switch r.Body.Kind {
	case "", "empty":
		if r.Body.Reference != nil || len(r.Body.Value) != 0 || r.Body.ContentType != "" {
			return ErrInvalid
		}
	case "json":
		if len(r.Body.Value) == 0 || len(r.Body.Value) > 256*1024 || r.Body.Reference != nil || r.Body.ContentType != "" || !boundedJSON(r.Body.Value) {
			return ErrInvalid
		}
	case "text":
		var text string
		if json.Unmarshal(r.Body.Value, &text) != nil || len(text) > 256*1024 || r.Body.Reference != nil || !validMedia(r.Body.ContentType) {
			return ErrInvalid
		}
	case "artifact":
		if r.Body.Reference == nil || len(r.Body.Value) != 0 || !ValidArtifact(*r.Body.Reference, MaxResponse) || !validMedia(r.Body.ContentType) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if (r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS") && r.Body.Kind != "" && r.Body.Kind != "empty" {
		return ErrInvalid
	}
	return nil
}
func ValidArtifact(a Artifact, limit uint64) bool {
	return a.Reference != "" && len(a.Reference) <= 1024 && a.ImmutableVersion != "" && len(a.ImmutableVersion) <= 1024 && strings.TrimSpace(a.Reference) != "" && strings.TrimSpace(a.ImmutableVersion) != "" && strings.IndexFunc(a.Reference+a.ImmutableVersion, unicode.IsControl) < 0 && ValidDigest(a.SHA256) && a.ByteLength <= limit
}
func validMedia(v string) bool {
	if len(v) > 128 {
		return false
	}
	kind, subtype, found := strings.Cut(v, "/")
	valid := func(part string) bool {
		if part == "" {
			return false
		}
		for _, c := range []byte(part) {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$&^_.+-", rune(c)) {
				return false
			}
		}
		return true
	}
	return found && valid(kind) && valid(subtype)
}
func validHeaderName(v string) bool {
	if v == "" {
		return false
	}
	for _, c := range []byte(v) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}
func RestrictedHeader(name string) bool {
	if strings.HasPrefix(name, "x-elitea-") {
		return true
	}
	switch name {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "host", "connection", "content-length", "content-type", "transfer-encoding", "te", "trailer", "upgrade", "proxy-connection", "keep-alive", "idempotency-key":
		return true
	}
	return false
}
func boundedJSON(data []byte) bool {
	if !strictJSON(data, 32, 16384) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF {
		return false
	}
	count := 0
	var walk func(any, int) bool
	walk = func(v any, depth int) bool {
		count++
		if depth > 32 || count > 16384 {
			return false
		}
		switch v := v.(type) {
		case []any:
			for _, child := range v {
				if !walk(child, depth+1) {
					return false
				}
			}
		case map[string]any:
			for _, child := range v {
				if !walk(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return walk(value, 0)
}
func Project(r Request, status uint16, contentType string, body []byte, artifact *Artifact) (*Projection, string) {
	if status < 200 || status > 599 {
		return nil, "invalid_response"
	}
	if status >= 300 && status <= 399 {
		return nil, "redirect_refused"
	}
	if status == 401 {
		return nil, "authentication"
	}
	if status == 403 {
		return nil, "authorization"
	}
	accepted := false
	for _, v := range r.Response.AcceptedStatuses {
		if status >= v.First && status <= v.Last {
			accepted = true
		}
	}
	if !accepted {
		switch {
		case status == 429:
			return nil, "rate_limited"
		case status == 408 || status == 504:
			return nil, "timeout"
		case status >= 500:
			return nil, "dependency_unavailable"
		default:
			return nil, "unexpected_status"
		}
	}
	size := uint64(len(body))
	if artifact != nil {
		size = artifact.ByteLength
	}
	if size > r.Response.MaxBytes {
		return nil, "resource_exhausted"
	}
	if r.Method == http.MethodHead || status == 204 || status == 205 {
		if size != 0 {
			return nil, "invalid_response"
		}
		return &Projection{Status: status, Data: Data{Kind: "empty"}}, ""
	}
	media, _, err := mime.ParseMediaType(contentType)
	media = strings.ToLower(media)
	if err != nil || len(contentType) > 512 {
		return nil, "invalid_response"
	}
	result := &Projection{Status: status, ContentType: &media, ByteLength: size}
	if r.Response.Mode == "artifact" {
		if artifact == nil || !ValidArtifact(*artifact, r.Response.MaxBytes) {
			return nil, "invalid_response"
		}
		result.Data = Data{Kind: "artifact", Reference: artifact}
		return result, ""
	}
	if artifact != nil || len(body) > MaxInline {
		return nil, "resource_exhausted"
	}
	switch r.Response.Mode {
	case "json":
		if media != "application/json" && !strings.HasSuffix(media, "+json") || !boundedJSON(body) {
			return nil, "invalid_response"
		}
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		var v any
		if d.Decode(&v) != nil {
			return nil, "invalid_response"
		}
		result.Data = Data{Kind: "json", Value: v}
	case "text":
		if !utf8.Valid(body) || (!strings.HasPrefix(media, "text/") && media != "application/json" && media != "application/xml" && !strings.HasSuffix(media, "+json") && !strings.HasSuffix(media, "+xml")) {
			return nil, "invalid_response"
		}
		result.Data = Data{Kind: "text", Value: string(body)}
	default:
		return nil, "invalid_response"
	}
	return result, ""
}

// JSON null remains a present typed value.
func (d Data) MarshalJSON() ([]byte, error) {
	switch d.Kind {
	case "json", "text":
		return json.Marshal(struct {
			Kind  string `json:"kind"`
			Value any    `json:"value"`
		}{d.Kind, d.Value})
	case "artifact":
		return json.Marshal(struct {
			Kind      string    `json:"kind"`
			Reference *Artifact `json:"reference"`
		}{d.Kind, d.Reference})
	default:
		return json.Marshal(struct {
			Kind string `json:"kind"`
		}{d.Kind})
	}
}

// Strict JSON refuses duplicate fields before either side can assign authority.
func strictJSON(data []byte, maxDepth, maxNodes int) bool {
	if !utf8.Valid(data) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	count := 0
	var value func(int) bool
	value = func(depth int) bool {
		count++
		if depth > maxDepth || count > maxNodes {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, container := token.(json.Delim)
		if !container {
			return true
		}
		switch delim {
		case '{':
			names := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || names[name] {
					return false
				}
				names[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func DecodeReceipt(data []byte) (Receipt, error) {
	if len(data) == 0 || len(data) > 3*1024*1024 || !strictJSON(data, 40, 20000) {
		return Receipt{}, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 8 {
		return Receipt{}, ErrInvalid
	}
	for _, name := range []string{"schema_version", "activation_id", "request_digest", "binding_digest", "effect_id", "state", "failure_code", "result"} {
		if _, ok := fields[name]; !ok {
			return Receipt{}, ErrInvalid
		}
	}
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.SchemaVersion != ReceiptSchema || !ValidDigest(receipt.ActivationID) || !ValidDigest(receipt.RequestDigest) || !ValidDigest(receipt.BindingDigest) || !ValidDigest(receipt.EffectID) {
		return Receipt{}, ErrInvalid
	}
	return receipt, nil
}
