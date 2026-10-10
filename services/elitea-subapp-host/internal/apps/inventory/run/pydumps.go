package run

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// pyPair / pyObject are a JSON object whose keys keep insertion order. A Go
// map serialises with sorted keys, which is not what Python's json.dumps (and
// so the Rust engine's pyjson::dumps) writes; the fixture artifacts are
// compared byte for byte against the Rust fixture's, so they are built from
// pyObject and written by pyDumps.
type pyPair struct {
	Key   string
	Value any
}

type pyObject []pyPair

// pyDumps is Python's json.dumps(value) for the value kinds the fixtures
// hold (pyObject, []any, []string, string, bool, int, nil): one line, ", "
// and ": " separators, ensure_ascii=True, no HTML escaping.
func pyDumps(value any) string {
	var out strings.Builder
	writePy(&out, value)
	return out.String()
}

func writePy(out *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(v))
	case int:
		out.WriteString(strconv.Itoa(v))
	case string:
		writePyString(out, v)
	case []string:
		items := make([]any, len(v))
		for i, item := range v {
			items[i] = item
		}
		writePy(out, items)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteString(", ")
			}
			writePy(out, item)
		}
		out.WriteByte(']')
	case pyObject:
		out.WriteByte('{')
		for i, pair := range v {
			if i > 0 {
				out.WriteString(", ")
			}
			writePyString(out, pair.Key)
			out.WriteString(": ")
			writePy(out, pair.Value)
		}
		out.WriteByte('}')
	default:
		panic(fmt.Sprintf("pyDumps: unsupported %T", value))
	}
}

func writePyString(out *strings.Builder, text string) {
	out.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r < 0x20 || (r > 0x7f && r < 0x10000):
			fmt.Fprintf(out, `\u%04x`, r)
		case r >= 0x10000:
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(out, `\u%04x\u%04x`, high, low)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
}
