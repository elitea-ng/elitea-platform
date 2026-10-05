package storage

// Shared bounded declaration JSON helpers. These functions do not grant a
// debug artifact, workspace, sandbox execution, or platform operation.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

func CodeDebugSHA256(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func debugHex(s string) bool            { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
func strictCodeDebugJSON(raw []byte, target any) error {
	if uniqueCodeDebugJSON(raw) != nil {
		return ErrContentRejected
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return ErrContentRejected
	}
	return nil
}
func codeDebugJSONObject(raw []byte) (map[string]any, error) {
	if uniqueCodeDebugJSON(raw) != nil {
		return nil, ErrContentRejected
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value map[string]any
	if d.Decode(&value) != nil || value == nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrContentRejected
	}
	return value, nil
}
func codeDebugNodeID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.:", c)) {
			return false
		}
	}
	return true
}

func uniqueCodeDebugJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 132 {
			return ErrContentRejected
		}
		t, e := d.Token()
		if e != nil {
			return ErrContentRejected
		}
		mark, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch mark {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, e := d.Token()
				k, ok := t.(string)
				if e != nil || !ok || seen[k] {
					return ErrContentRejected
				}
				seen[k] = true
				if value(depth+1) != nil {
					return ErrContentRejected
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return ErrContentRejected
			}
		case '[':
			for d.More() {
				if value(depth+1) != nil {
					return ErrContentRejected
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return ErrContentRejected
			}
		default:
			return ErrContentRejected
		}
		return nil
	}
	if value(0) != nil {
		return ErrContentRejected
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrContentRejected
	}
	return nil
}
