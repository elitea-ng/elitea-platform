package httpaction

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
)

// CanonicalJSON returns the contract canonical form of one strict JSON value
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md §7): object keys
// sorted by UTF-8 bytes at every level, compact, numbers kept as their exact
// text, strings escaped only for '"', '\\' and controls below U+0020 (short
// forms for \b \t \n \f \r, otherwise \u00xx lowercase), everything else raw
// UTF-8, no trailing newline. encoding/json is not used for output: it escapes
// U+2028, U+2029 and <>&, which the Worker writer does not.
//
// The input must already be within the caller's byte budget; duplicate keys,
// invalid UTF-8, depth above 32 or more than 16384 nodes are refused.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if !strictJSON(raw, 32, 16384) {
		return nil, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	var out bytes.Buffer
	out.Grow(len(raw))
	writeCanonical(&out, value)
	return out.Bytes(), nil
}

// canonicalValue writes an already-decoded value (json.Number numbers).
func canonicalValue(value any) []byte {
	var out bytes.Buffer
	writeCanonical(&out, value)
	return out.Bytes()
}

func writeCanonical(out *bytes.Buffer, value any) {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case json.Number:
		out.WriteString(typed.String())
	case string:
		writeCanonicalString(out, typed)
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			writeCanonical(out, item)
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
			writeCanonical(out, typed[key])
		}
		out.WriteByte('}')
	}
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
