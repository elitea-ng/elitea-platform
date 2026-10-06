package noderecovery

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// DecodeStrictNullableObject admits null only for named top-level protocol fields.
// Callers must validate each non-null nested object with its typed decoder.
func DecodeStrictNullableObject(raw []byte, target any, required, nullable []string) error {
	if len(raw) == 0 || len(raw) > 8192 || !utf8.Valid(raw) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if walkNullable(d, 0) != nil {
		return ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(required) {
		return ErrInvalid
	}
	allowed := map[string]bool{}
	for _, key := range nullable {
		allowed[key] = true
	}
	for _, key := range required {
		value, ok := fields[key]
		if !ok || (!allowed[key] && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return ErrInvalid
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return ErrInvalid
	}
	return nil
}

func walkNullable(d *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrInvalid
	}
	t, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	delimiter, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok || seen[key] {
				return ErrInvalid
			}
			seen[key] = true
			if walkNullable(d, depth+1) != nil {
				return ErrInvalid
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for d.More() {
			if walkNullable(d, depth+1) != nil {
				return ErrInvalid
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
