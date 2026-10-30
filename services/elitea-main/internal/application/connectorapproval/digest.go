package connectorapproval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ToolDigest is the pin of one MCP tool: the lower-case hex SHA-256 of the
// canonical JSON (CanonicalJSON) of
//
//	{"description": <description>, "inputSchema": <input schema>, "name": <name>}
//
// The worker computes the same digest over the server's live listing
// (libs/rust/agent-runtime/src/toolkits/mcp_pin.rs) and keeps only the tools
// whose name and digest an approved request pinned. A change to a tool's
// description (a prompt-injection vector) or its arguments is a different
// digest, so the changed tool is hidden until it is approved again. The two
// implementations share a test vector (digest_test.go, mcp_pin.rs).
func ToolDigest(name, description string, inputSchema any) (string, error) {
	canonical, err := CanonicalJSON(map[string]any{
		"description": description,
		"inputSchema": inputSchema,
		"name":        name,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalJSON writes a JSON value with object keys sorted bytewise, no
// insignificant whitespace, strings escaped as serde_json escapes them
// (`"`, `\` and the C0 controls only; \b \t \n \f \r, else \u00xx in lower
// case; everything else raw UTF-8) and EVERY number in one form, however it
// was written or decoded (es6Number): `0.0`, `1E+3`, `1e-07` and `-0` are
// `0`, `1000`, `1e-7` and `0`. The worker writes numbers by the same rule
// (mcp_pin.rs es6_number); a number outside the double range is an error.
// A value that is not JSON is an error.
func CanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeCanonical(&buffer, value, 0); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

const maxCanonicalDepth = 64

var errNotJSON = errors.New("the value is not JSON")

func writeCanonical(buffer *bytes.Buffer, value any, depth int) error {
	if depth > maxCanonicalDepth {
		return fmt.Errorf("the value nests deeper than %d levels", maxCanonicalDepth)
	}
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		writeCanonicalString(buffer, typed)
	case json.Number:
		parsed, err := strconv.ParseFloat(typed.String(), 64)
		if err != nil {
			return errNotJSON
		}
		return writeCanonicalNumber(buffer, parsed)
	case float64:
		return writeCanonicalNumber(buffer, typed)
	case int:
		return writeCanonicalNumber(buffer, float64(typed))
	case int64:
		return writeCanonicalNumber(buffer, float64(typed))
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeCanonical(buffer, item, depth+1); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			writeCanonicalString(buffer, key)
			buffer.WriteByte(':')
			if err := writeCanonical(buffer, typed[key], depth+1); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		// Any other Go value (a typed struct or slice) goes through its JSON
		// form first.
		encoded, err := json.Marshal(typed)
		if err != nil {
			return errNotJSON
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var generic any
		if err := decoder.Decode(&generic); err != nil {
			return errNotJSON
		}
		return writeCanonical(buffer, generic, depth)
	}
	return nil
}

func writeCanonicalNumber(buffer *bytes.Buffer, value float64) error {
	text, ok := es6Number(value)
	if !ok {
		return errNotJSON
	}
	buffer.WriteString(text)
	return nil
}

// es6Number is ECMAScript's Number.prototype.toString of a finite double:
// the shortest digits that round-trip, positional for 1e-7 <= |x| < 1e21,
// otherwise d.ddde±n; -0 is 0. It is the one number form of the tool digest,
// so a schema read as a float64 (Main), a JavaScript number (the request
// form) or number text (the worker) digests the same.
func es6Number(value float64) (string, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", false
	}
	if value == 0 {
		return "0", true
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64) // -1.2345e-07
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exponent, err := strconv.Atoi(exponentText)
	if err != nil {
		return "", false
	}
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign, mantissa = "-", mantissa[1:]
	}
	digits := strings.Replace(mantissa, ".", "", 1)
	k, n := len(digits), exponent+1
	var body string
	switch {
	case k <= n && n <= 21:
		body = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		body = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		body = "0." + strings.Repeat("0", -n) + digits
	default:
		e := n - 1
		exp := strconv.Itoa(e)
		if e >= 0 {
			exp = "+" + exp
		}
		if k == 1 {
			body = digits + "e" + exp
		} else {
			body = digits[:1] + "." + digits[1:] + "e" + exp
		}
	}
	return sign + body, true
}

func writeCanonicalString(buffer *bytes.Buffer, text string) {
	const hexDigits = "0123456789abcdef"
	if !utf8.ValidString(text) {
		text = string([]rune(text))
	}
	buffer.WriteByte('"')
	for index := 0; index < len(text); index++ {
		c := text[index]
		switch c {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\t':
			buffer.WriteString(`\t`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\r':
			buffer.WriteString(`\r`)
		default:
			if c < 0x20 {
				buffer.WriteString(`\u00`)
				buffer.WriteByte(hexDigits[c>>4])
				buffer.WriteByte(hexDigits[c&0xf])
			} else {
				buffer.WriteByte(c)
			}
		}
	}
	buffer.WriteByte('"')
}

// DecodeJSONValue decodes a JSON document keeping numbers as written, the
// form CanonicalJSON digests.
func DecodeJSONValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}
