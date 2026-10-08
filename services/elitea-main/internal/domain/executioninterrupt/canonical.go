package executioninterrupt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

var errNotCanonicalJSON = errors.New("JSON value is not accepted")

// Canonicalize strictly decodes one JSON value of at most maxBytes and writes
// the contract canonical form (contract §7): object keys sorted by UTF-8 bytes
// at every level, compact, exact integers, strings escaped only for '"', '\\'
// and controls below U+0020, every other character raw UTF-8, no trailing
// newline. Go encoding/json always escapes U+2028 and U+2029, so it is not used.
//
// It refuses invalid UTF-8, duplicate keys at any level, nesting deeper than
// MaxJSONDepth, trailing data, and any number that is not a canonical int64
// (no schema in the family carries a fraction).
func Canonicalize(raw []byte, maxBytes int) ([]byte, error) {
	value, err := decodeStrict(raw, maxBytes)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(len(raw))
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decodeStrict(raw []byte, maxBytes int) (any, error) {
	if len(raw) == 0 || len(raw) > maxBytes || !utf8.Valid(raw) {
		return nil, errNotCanonicalJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := readValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errNotCanonicalJSON
	}
	return value, nil
}

func readValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, errNotCanonicalJSON
	}
	switch typed := token.(type) {
	case json.Delim:
		if depth >= MaxJSONDepth {
			return nil, errNotCanonicalJSON
		}
		switch typed {
		case '{':
			object := map[string]any{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok {
					return nil, errNotCanonicalJSON
				}
				if _, duplicate := object[key]; duplicate {
					return nil, errNotCanonicalJSON
				}
				member, err := readValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = member
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, errNotCanonicalJSON
			}
			return object, nil
		case '[':
			array := []any{}
			for decoder.More() {
				item, err := readValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, item)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return nil, errNotCanonicalJSON
			}
			return array, nil
		}
		return nil, errNotCanonicalJSON
	case json.Number:
		if !canonicalInteger(typed.String()) {
			return nil, errNotCanonicalJSON
		}
		return typed, nil
	case string, bool, nil:
		return typed, nil
	}
	return nil, errNotCanonicalJSON
}

func canonicalInteger(text string) bool {
	if text == "" || text == "-0" {
		return false
	}
	digits := text
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

// writeCanonical writes a value built from nil, bool, json.Number, int64,
// string, []any and map[string]any. Any other type is refused, never guessed.
func writeCanonical(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case json.Number:
		out.WriteString(typed.String())
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case string:
		writeCanonicalString(out, typed)
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeCanonicalString(out, key)
			out.WriteByte(':')
			if err := writeCanonical(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return errNotCanonicalJSON
	}
	return nil
}

func writeCanonicalString(out *bytes.Buffer, value string) {
	const hexDigits = "0123456789abcdef"
	out.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if character < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[character>>4])
				out.WriteByte(hexDigits[character&0xf])
			} else {
				out.WriteRune(character)
			}
		}
	}
	out.WriteByte('"')
}

// canonicalOf writes a package-built value canonically.
func canonicalOf(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
