package outputcontract

// The worker compiles a regex contract with the Rust `regex` crate under a
// 1 MiB compiled size limit (MAX_REGEX_SIZE in
// libs/rust/agent-runtime/src/output_contract.rs). Go RE2 accepts some
// patterns that the Rust crate refuses, and some patterns have a different
// meaning in the two. This file refuses them at save time, so that a saved
// contract always compiles in the worker. The shared fixture
// libs/proto/contracts/output-contract-regex-v1.json holds both sides to the
// same list.

import (
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxRegexSize is the largest estimated compiled size of a pattern, in
	// bytes. The worker refuses a pattern whose compiled program exceeds
	// 1 MiB. The estimate is not smaller than the size that the Rust crate
	// measures (calibrated on every Unicode class), so this bound keeps a
	// margin of one quarter.
	MaxRegexSize = 768 * 1024
	// MaxRegexDepth is the deepest nesting of groups, and of the parsed
	// pattern. The worker refuses a pattern nested deeper than 250 levels.
	MaxRegexDepth = 100
)

// counted is a valid counted repetition: {n}, {n,} or {n,m}.
var counted = regexp.MustCompile(`^\{[0-9]+(,[0-9]*)?\}`)

// Unicode replacements of the Perl classes, for the cost estimate only. In
// Go, \d, \s and \w are ASCII; in the worker they are Unicode and much
// larger. A negated class costs about the same as the class.
var perlCost = map[byte][2]string{
	'd': {`\p{Nd}`, `[\p{Nd}]`},
	'D': {`\P{Nd}`, `[\P{Nd}]`},
	's': {whiteSpace, `[` + whiteSpace + `]`},
	'S': {`\x{0}-\x{10FFFF}`, `[^` + whiteSpace + `]`},
	'w': {word, `[` + word + `]`},
	'W': {word, `[^` + word + `]`},
}

const (
	whiteSpace = `\t-\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}`
	word       = `\p{L}\p{M}\p{Nd}\p{Pc}\p{Nl}\p{So}\x{200C}\x{200D}`
)

// checkRegex reports whether Main accepts pattern. An accepted pattern
// compiles in the worker (wrapped as ^(?:pattern)$). It refuses:
//
//   - syntax that only Go has: \Q...\E, octal escapes (\0, \012), a { that
//     does not start a counted repetition, a capture name that starts with
//     a digit, a repeated capture name, \p{^...}, the surrogate category
//     (\p{Cs}) and surrogate code points (\x{D800});
//   - syntax that has a different meaning in the worker: \< and \>, \b{...},
//     a nested [ in a class, && ~~ and -- in a class, and a range that starts
//     at a class (\d-z);
//   - a pattern nested deeper than MaxRegexDepth or with an estimated
//     compiled size above MaxRegexSize.
func checkRegex(pattern string) bool {
	costPattern, ok := scanRegex(pattern)
	if !ok {
		return false
	}
	parsed, err := syntax.Parse("^(?:"+pattern+")$", syntax.Perl)
	if err != nil {
		return false
	}
	names := map[string]bool{}
	if !uniqueNames(parsed, names) || height(parsed) > MaxRegexDepth {
		return false
	}
	costTree, err := syntax.Parse(costPattern, syntax.Perl)
	if err != nil {
		return false
	}
	return cost(costTree.Simplify(), MaxRegexSize+1) <= MaxRegexSize
}

// scanRegex applies the token rules and returns the pattern with the Perl
// classes replaced by their Unicode size equivalents.
func scanRegex(pattern string) (string, bool) {
	var out strings.Builder
	inClass := false
	classBody := 0
	groups := 0
	afterClassEscape := false
	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch {
		case c == '\\':
			next, end, ok := scanEscape(pattern, i, inClass)
			if !ok {
				return "", false
			}
			afterClassEscape = next != [2]string{}
			switch {
			case !afterClassEscape:
				out.WriteString(pattern[i:end])
			case inClass:
				out.WriteString(next[0])
			default:
				out.WriteString(next[1])
			}
			i = end
			continue
		case inClass:
			rest := pattern[i:]
			switch {
			case c == ']' && i > classBody:
				inClass = false
			case c == '[' && strings.HasPrefix(rest, "[:"):
				end := strings.Index(rest, ":]")
				if end < 0 {
					return "", false
				}
				out.WriteString(rest[:end+2])
				i += end + 2
				afterClassEscape = false
				continue
			case c == '[':
				return "", false
			case strings.HasPrefix(rest, "&&"), strings.HasPrefix(rest, "~~"), strings.HasPrefix(rest, "--"):
				return "", false
			case c == '-' && afterClassEscape && !strings.HasPrefix(rest, "-]"):
				return "", false
			}
		case c == '[':
			inClass = true
			classBody = i + 1
			if strings.HasPrefix(pattern[i+1:], "^") {
				classBody++
			}
		case c == '{':
			if !counted.MatchString(pattern[i:]) {
				return "", false
			}
		case c == '(':
			groups++
			if groups > MaxRegexDepth || !captureNameOK(pattern[i:]) {
				return "", false
			}
		case c == ')':
			groups--
		}
		afterClassEscape = false
		_, size := utf8.DecodeRuneInString(pattern[i:])
		out.WriteString(pattern[i : i+size])
		i += size
	}
	return out.String(), true
}

// scanEscape checks the escape at pattern[i]. It returns the Unicode size
// equivalent of a class escape (empty for any other escape) and the index
// after the escape.
func scanEscape(pattern string, i int, inClass bool) ([2]string, int, bool) {
	none := [2]string{}
	if i+1 >= len(pattern) {
		return none, 0, false
	}
	next := pattern[i+1]
	switch {
	case next == 'Q' || next == 'E' || next == '<' || next == '>':
		return none, 0, false
	case next >= '0' && next <= '9':
		return none, 0, false
	case (next == 'b' || next == 'B') && !inClass && strings.HasPrefix(pattern[i+2:], "{"):
		return none, 0, false
	case next == 'x':
		end, ok := scanHex(pattern, i)
		return none, end, ok
	case next == 'p' || next == 'P':
		end, ok := scanProperty(pattern, i)
		escape := pattern[i:min(end, len(pattern))]
		return [2]string{escape, escape}, end, ok
	}
	if equivalent, ok := perlCost[next]; ok {
		return equivalent, i + 2, true
	}
	_, size := utf8.DecodeRuneInString(pattern[i+1:])
	return none, i + 1 + size, true
}

// scanHex refuses a \x{...} escape of a surrogate code point.
func scanHex(pattern string, i int) (int, bool) {
	rest := pattern[i+2:]
	if !strings.HasPrefix(rest, "{") {
		return min(i+4, len(pattern)), true
	}
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		return 0, false
	}
	value, err := strconv.ParseUint(rest[1:end], 16, 32)
	if err != nil || (value >= 0xD800 && value <= 0xDFFF) {
		return 0, false
	}
	return i + 2 + end + 1, true
}

// scanProperty refuses \p{^...} and the surrogate category.
func scanProperty(pattern string, i int) (int, bool) {
	rest := pattern[i+2:]
	if rest == "" {
		return 0, false
	}
	name, end := rest[:1], i+3
	if rest[0] == '{' {
		closing := strings.IndexByte(rest, '}')
		if closing < 0 {
			return 0, false
		}
		name, end = rest[1:closing], i+2+closing+1
	}
	if strings.HasPrefix(name, "^") || strings.EqualFold(name, "Cs") || strings.EqualFold(name, "Surrogate") {
		return 0, false
	}
	return end, true
}

// captureNameOK refuses a capture name that starts with a digit.
func captureNameOK(rest string) bool {
	var name string
	switch {
	case strings.HasPrefix(rest, "(?P<"):
		name = rest[4:]
	case strings.HasPrefix(rest, "(?<"):
		name = rest[3:]
	default:
		return true
	}
	return name == "" || name[0] < '0' || name[0] > '9'
}

func uniqueNames(re *syntax.Regexp, names map[string]bool) bool {
	if re.Op == syntax.OpCapture && re.Name != "" {
		if names[re.Name] {
			return false
		}
		names[re.Name] = true
	}
	for _, sub := range re.Sub {
		if !uniqueNames(sub, names) {
			return false
		}
	}
	return true
}

func height(re *syntax.Regexp) int {
	deepest := 0
	for _, sub := range re.Sub {
		deepest = max(deepest, height(sub))
	}
	return deepest + 1
}

// The weights of the size estimate, in bytes, measured against the Rust
// crate (regex 1.13): a pattern, a node that is not a leaf, one UTF-8 byte of
// a literal, a class, one byte transition of a class and one byte sequence
// of a class.
const (
	patternBytes    = 512
	nodeBytes       = 96
	literalBytes    = 40
	classBytes      = 128
	transitionBytes = 2
	sequenceBytes   = 112
)

// cost estimates the compiled size of re in bytes. It stops counting above
// limit.
func cost(re *syntax.Regexp, limit int) int {
	return patternBytes + nodeCost(re, limit)
}

func nodeCost(re *syntax.Regexp, limit int) int {
	switch re.Op {
	case syntax.OpLiteral:
		total := 0
		for _, r := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 {
				total += classCost(foldOrbit(r))
			} else {
				total += literalBytes * utf8.RuneLen(r)
			}
		}
		return total
	case syntax.OpCharClass:
		return classCost(re.Rune)
	case syntax.OpAnyCharNotNL:
		return classCost([]rune{0, '\n' - 1, '\n' + 1, unicode.MaxRune})
	case syntax.OpAnyChar:
		return classCost([]rune{0, unicode.MaxRune})
	case syntax.OpRepeat:
		count := re.Max
		if count < 0 {
			count = re.Min + 1
		}
		return min(limit, nodeCost(re.Sub[0], limit)*max(count, 1)+nodeBytes)
	}
	total := nodeBytes
	for _, sub := range re.Sub {
		total += nodeCost(sub, limit)
		if total > limit {
			return total
		}
	}
	return total
}

// foldOrbit is the class of the runes that are equal to r without case.
func foldOrbit(r rune) []rune {
	orbit := []rune{r, r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		orbit = append(orbit, f, f)
	}
	return orbit
}

// classCost estimates the compiled size of a class from its UTF-8
// automaton: the byte sequences that the worker compiles for each range.
func classCost(ranges []rune) int {
	transitions, sequences := 0, 0
	for i := 0; i+1 < len(ranges); i += 2 {
		utf8Sequences(ranges[i], ranges[i+1], func(length int) {
			transitions += length
			sequences++
		})
	}
	return classBytes + transitionBytes*transitions + sequenceBytes*sequences
}

// utf8Sequences splits [lo, hi] into ranges of one UTF-8 length, and these
// into ranges with one byte sequence, as a UTF-8 automaton compiler does. It
// calls emit with the length of each sequence.
func utf8Sequences(lo, hi rune, emit func(int)) {
	if lo >= 0xD800 && lo <= 0xDFFF {
		lo = 0xE000
	}
	if hi >= 0xD800 && hi <= 0xDFFF {
		hi = 0xD7FF
	}
	if lo > hi {
		return
	}
	if lo < 0xD800 && hi > 0xDFFF {
		utf8Sequences(lo, 0xD7FF, emit)
		utf8Sequences(0xE000, hi, emit)
		return
	}
	for _, edge := range []rune{0x7F, 0x7FF, 0xFFFF} {
		if lo <= edge && hi > edge {
			utf8Sequences(lo, edge, emit)
			utf8Sequences(edge+1, hi, emit)
			return
		}
	}
	n := utf8.RuneLen(lo)
	if n < 0 {
		return
	}
	for i := 1; i < n; i++ {
		mask := rune(1)<<(6*i) - 1
		if lo&^mask == hi&^mask {
			continue
		}
		if lo&mask != 0 {
			utf8Sequences(lo, lo|mask, emit)
			utf8Sequences((lo|mask)+1, hi, emit)
			return
		}
		if hi&mask != mask {
			utf8Sequences(lo, (hi&^mask)-1, emit)
			utf8Sequences(hi&^mask, hi, emit)
			return
		}
	}
	emit(n)
}
