// Package clientversion parses and orders native client versions (ADR-0025
// decision 5: `min_client_version` and the `X-Client-Version` header).
//
// The grammar is Semantic Versioning 2.0.0's: MAJOR.MINOR.PATCH, an optional
// `-pre.release` and an optional `+build` suffix, which is ignored when
// ordering. Two departures, both lenient on purpose because the input is a
// header an app store build sends and refusing it means a 400 on every call:
// numeric identifiers may carry leading zeros, and a leading `v` is accepted.
//
// It is ~100 lines instead of golang.org/x/mod/semver because that module is
// not a direct dependency of elitea-main, and its grammar requires the `v`.
package clientversion

import (
	"errors"
	"strconv"
	"strings"
)

// MaxLength bounds an accepted version string. A header longer than this is
// malformed, not truncated.
const MaxLength = 64

// ErrMalformed is any string that is not a version.
var ErrMalformed = errors.New("clientversion: not MAJOR.MINOR.PATCH[-prerelease][+build]")

// Version is a parsed version. Build metadata is dropped.
type Version struct {
	Major, Minor, Patch uint64
	// Pre holds the dot-separated pre-release identifiers; empty for a release.
	Pre []string
}

// Parse reads one version.
func Parse(text string) (Version, error) {
	if text == "" || len(text) > MaxLength {
		return Version{}, ErrMalformed
	}
	text = strings.TrimPrefix(text, "v")
	if plus := strings.IndexByte(text, '+'); plus >= 0 {
		build := text[plus+1:]
		if !validIdentifiers(build) {
			return Version{}, ErrMalformed
		}
		text = text[:plus]
	}
	var pre []string
	if dash := strings.IndexByte(text, '-'); dash >= 0 {
		release := text[dash+1:]
		if !validIdentifiers(release) {
			return Version{}, ErrMalformed
		}
		pre = strings.Split(release, ".")
		text = text[:dash]
	}
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return Version{}, ErrMalformed
	}
	var numbers [3]uint64
	for i, part := range parts {
		if !allDigits(part) || len(part) > 9 {
			return Version{}, ErrMalformed
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return Version{}, ErrMalformed
		}
		numbers[i] = n
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2], Pre: pre}, nil
}

// Valid reports whether text parses.
func Valid(text string) bool {
	_, err := Parse(text)
	return err == nil
}

// Compare orders a and b: -1, 0 or +1, by SemVer 2.0.0 §11 precedence.
func Compare(a, b Version) int {
	for _, pair := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1 // a release outranks its pre-releases
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdentifier(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

// Below reports whether version orders strictly before minimum. Both must
// parse; an empty minimum is "no minimum" and never refuses.
func Below(version, minimum string) (bool, error) {
	if minimum == "" {
		return false, nil
	}
	floor, err := Parse(minimum)
	if err != nil {
		return false, err
	}
	got, err := Parse(version)
	if err != nil {
		return false, err
	}
	return Compare(got, floor) < 0, nil
}

// Max returns the higher of two version strings. An empty or unparsable
// operand loses to a parsable one; two unusable operands give "".
func Max(a, b string) string {
	va, errA := Parse(a)
	vb, errB := Parse(b)
	switch {
	case errA != nil && errB != nil:
		return ""
	case errA != nil:
		return b
	case errB != nil:
		return a
	case Compare(va, vb) >= 0:
		return a
	default:
		return b
	}
}

func compareIdentifier(a, b string) int {
	aNumeric, bNumeric := allDigits(a), allDigits(b)
	switch {
	case aNumeric && bNumeric:
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case aNumeric:
		return -1 // numeric identifiers have lower precedence
	case bNumeric:
		return 1
	}
	return strings.Compare(a, b)
}

// validIdentifiers checks a dot-separated list of [0-9A-Za-z-]+ identifiers.
func validIdentifiers(text string) bool {
	if text == "" {
		return false
	}
	for _, identifier := range strings.Split(text, ".") {
		if identifier == "" {
			return false
		}
		for _, r := range identifier {
			if !identifierRune(r) {
				return false
			}
		}
	}
	return true
}

func identifierRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-'
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
