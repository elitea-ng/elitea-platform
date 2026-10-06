// Package codeplatform defines the credential-free Code request boundary.
package codeplatform

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Revision         = 1
	MaxRequestHeader = 262144
	MaxReplyHeader   = 2097152
	MaxChunk         = 65536
	MaxObject        = 8388608
	MaxCalls         = 4096
	MaxDepth         = 32
	MaxValues        = 10000
)

var ErrFrame = errors.New("invalid Code platform frame")
var idPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Request retains exact received bytes for immutable identity.
// No caller field can select a project, actor, grant, or transport endpoint.
type Request struct {
	Sequence   uint64
	Operation  string
	Resource   map[string]string
	Arguments  json.RawMessage
	Payload    []byte
	ExactFrame []byte
}

type requestHeader struct {
	Revision  uint32            `json:"revision"`
	Sequence  uint64            `json:"sequence"`
	Operation string            `json:"operation"`
	Resource  map[string]string `json:"resource"`
	Arguments json.RawMessage   `json:"arguments"`
}

func DecodeRequest(frame []byte) (Request, error) {
	if len(frame) < 8 || len(frame) > 8+MaxRequestHeader+MaxChunk {
		return Request{}, ErrFrame
	}
	headerBytes, bodyBytes := int(binary.BigEndian.Uint32(frame[:4])), int(binary.BigEndian.Uint32(frame[4:8]))
	if headerBytes == 0 || headerBytes > MaxRequestHeader || bodyBytes > MaxChunk || len(frame) != 8+headerBytes+bodyBytes {
		return Request{}, ErrFrame
	}
	raw := frame[8 : 8+headerBytes]
	if err := validateJSON(raw); err != nil {
		return Request{}, err
	}
	var header requestHeader
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil || header.Revision != Revision || header.Sequence == 0 || header.Sequence > MaxCalls || header.Arguments == nil {
		return Request{}, ErrFrame
	}
	request := Request{Sequence: header.Sequence, Operation: header.Operation, Resource: header.Resource, Arguments: header.Arguments}
	if err := validateOperation(request, bodyBytes); err != nil {
		return Request{}, err
	}
	request.Payload = append([]byte(nil), frame[8+headerBytes:]...)
	request.ExactFrame = append([]byte(nil), frame...)
	return request, nil
}

// CallDigest uses exact binary frame bytes under a frozen trusted job binding.
// Cross-language JSON number serialization cannot change this identity.
func (r Request) CallDigest(activation, prepared, policy [32]byte) [32]byte {
	hash := sha256.New()
	hash.Write([]byte("elitea.code.platform-call.v1\x00"))
	hash.Write(activation[:])
	hash.Write(prepared[:])
	hash.Write(policy[:])
	hash.Write(r.ExactFrame)
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func exact(resource map[string]string, keys ...string) bool {
	if len(resource) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, found := resource[key]; !found {
			return false
		}
	}
	return true
}
func positiveID(value string) bool {
	if !idPattern.MatchString(value) {
		return false
	}
	id, err := strconv.ParseInt(value, 10, 32)
	return err == nil && id > 0
}
func safeText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
func objectName(value string) bool {
	if !safeText(value, 1024) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func bucket(value string) bool {
	return safeText(value, 256) && !strings.ContainsAny(value, "/\\") && value != "." && value != ".."
}

func validateOperation(r Request, payload int) error {
	resource := r.Resource
	valid := false
	switch r.Operation {
	case "user_get":
		valid = exact(resource, "kind") && resource["kind"] == "current_user"
	case "application_list":
		valid = exact(resource, "kind") && resource["kind"] == "application_catalog"
	case "toolkit_list":
		valid = exact(resource, "kind") && resource["kind"] == "toolkit_catalog"
	case "application_get":
		valid = exact(resource, "kind", "id") && resource["kind"] == "application" && positiveID(resource["id"])
	case "application_version_get":
		valid = exact(resource, "kind", "application_id", "version_id") && resource["kind"] == "application_version" && positiveID(resource["application_id"]) && positiveID(resource["version_id"])
	case "toolkit_call":
		valid = exact(resource, "kind", "id", "revision", "tool") && resource["kind"] == "toolkit" && positiveID(resource["id"]) && digestPattern.MatchString(resource["revision"]) && safeText(resource["tool"], 1024)
	case "secret_read":
		valid = exact(resource, "kind", "scope", "name") && resource["kind"] == "secret" && (resource["scope"] == "project" || resource["scope"] == "personal") && len(resource["name"]) <= 256 && secretPattern.MatchString(resource["name"])
	case "bucket_exists", "bucket_create", "artifact_list":
		valid = exact(resource, "kind", "name") && resource["kind"] == "bucket" && bucket(resource["name"])
	case "artifact_head", "artifact_read", "artifact_write_begin", "artifact_append", "artifact_delete":
		valid = exact(resource, "kind", "bucket", "name") && resource["kind"] == "artifact" && bucket(resource["bucket"]) && objectName(resource["name"])
	case "artifact_read_chunk", "artifact_write_chunk", "artifact_write_commit":
		valid = exact(resource, "kind", "id") && resource["kind"] == "artifact_transfer" && digestPattern.MatchString(resource["id"])
	}
	if !valid || (payload != 0 && r.Operation != "artifact_write_chunk") {
		return ErrFrame
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(r.Arguments, &args) != nil || args == nil {
		return ErrFrame
	}
	if r.Operation == "toolkit_call" {
		return nil
	}
	keys := []string{}
	switch r.Operation {
	case "application_list", "toolkit_list":
		keys = []string{"cursor", "limit"}
	case "artifact_list":
		keys = []string{"prefix", "cursor", "limit"}
	case "artifact_read_chunk", "artifact_write_chunk":
		keys = []string{"offset"}
	case "artifact_write_begin":
		keys = []string{"bytes"}
	case "artifact_append":
		keys = []string{"text", "expected_version"}
	case "artifact_delete":
		keys = []string{"expected_version"}
	}
	if len(args) != len(keys) {
		return ErrFrame
	}
	for _, key := range keys {
		if _, exists := args[key]; !exists {
			return ErrFrame
		}
	}
	if _, present := args["limit"]; present {
		var limit int
		if json.Unmarshal(args["limit"], &limit) != nil || limit < 1 || limit > 100 {
			return ErrFrame
		}
		if !bytes.Equal(bytes.TrimSpace(args["cursor"]), []byte("null")) {
			var cursor string
			if json.Unmarshal(args["cursor"], &cursor) != nil || !digestPattern.MatchString(cursor) {
				return ErrFrame
			}
		}
	}
	if raw, present := args["offset"]; present {
		var offset uint64
		if json.Unmarshal(raw, &offset) != nil || offset > MaxObject {
			return ErrFrame
		}
	}
	if raw, present := args["bytes"]; present {
		var size uint64
		if json.Unmarshal(raw, &size) != nil || size > MaxObject {
			return ErrFrame
		}
	}
	if raw, present := args["expected_version"]; present {
		var version string
		if json.Unmarshal(raw, &version) != nil || !safeText(version, 256) {
			return ErrFrame
		}
	}
	if raw, present := args["text"]; present {
		var text string
		if json.Unmarshal(raw, &text) != nil || !utf8.ValidString(text) || len(text) > MaxChunk {
			return ErrFrame
		}
	}
	if raw, present := args["prefix"]; present {
		var prefix string
		if json.Unmarshal(raw, &prefix) != nil || len(prefix) > 1024 || (prefix != "" && !objectName(strings.TrimSuffix(prefix, "/"))) {
			return ErrFrame
		}
	}
	return nil
}

// validateJSON refuses duplicate keys before typed decode discards them.
func validateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	values := 0
	var value func(int) error
	value = func(depth int) error {
		values++
		if depth > MaxDepth || values > MaxValues {
			return ErrFrame
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrFrame
		}
		switch current := token.(type) {
		case json.Delim:
			if current != '{' && current != '[' {
				return ErrFrame
			}
			keys := make(map[string]struct{})
			for decoder.More() {
				if current == '{' {
					key, err := decoder.Token()
					if err != nil {
						return ErrFrame
					}
					name, ok := key.(string)
					if !ok {
						return ErrFrame
					}
					if _, present := keys[name]; present {
						return ErrFrame
					}
					keys[name] = struct{}{}
				}
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || (current == '{' && end != json.Delim('}')) || (current == '[' && end != json.Delim(']')) {
				return ErrFrame
			}
		case json.Number:
			number, err := strconv.ParseFloat(string(current), 64)
			if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
				return ErrFrame
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrFrame
	}
	return nil
}
