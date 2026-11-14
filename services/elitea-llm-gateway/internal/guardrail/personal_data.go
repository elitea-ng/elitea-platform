package guardrail

// The personal_data detector is the built-in personal data rule pack, next to
// the built-in secret rules (secrets.go). It finds four kinds of value:
//
//	email         an e-mail address
//	phone         an international number (a leading +) or a formatted North
//	              American number
//	payment-card  a card number of a known issuer that passes the Luhn check
//	iban          an IBAN of a registered country, with the registered length
//	              and a valid mod-97 check
//
// The pack is code, not data: RE2 has no look-around, and a number is only a
// finding after a checksum or a shape check. Each rule is a candidate regexp
// and a verifier. The verifier trades recall for precision on purpose: a bare
// run of digits is not a card, a date is not a phone number, and
// `icon@2x.png` is not an e-mail address. A person who needs a looser or a
// country-specific rule adds a regex detector or a Presidio detector beside
// it. A finding carries the rule id as its label and never the matched text.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Personal data rule ids, which are the labels the detector reports.
const (
	PersonalDataEmail = "email"
	PersonalDataPhone = "phone"
	PersonalDataCard  = "payment-card"
	PersonalDataIBAN  = "iban"
)

// PersonalDataRuleIDs lists the rule ids of the personal_data detector.
func PersonalDataRuleIDs() []string {
	return []string{PersonalDataEmail, PersonalDataPhone, PersonalDataCard, PersonalDataIBAN}
}

// personalRule finds candidates and verifies each. verify returns the span of
// the finding inside the candidate's bounds, or ok=false to drop it.
type personalRule struct {
	id     string
	res    []*regexp.Regexp
	verify func(content string, start, end int) (int, int, bool)
}

var personalRules = map[string]personalRule{
	PersonalDataEmail: {
		id:     PersonalDataEmail,
		res:    []*regexp.Regexp{regexp.MustCompile(`[A-Za-z0-9._%+-]{1,64}@(?:[A-Za-z0-9-]{1,63}\.)+[A-Za-z]{2,24}`)},
		verify: verifyEmail,
	},
	PersonalDataPhone: {
		id: PersonalDataPhone,
		res: []*regexp.Regexp{
			regexp.MustCompile(`\+[0-9][0-9 ().-]{6,20}[0-9]`),
			regexp.MustCompile(`\(?[2-9][0-9]{2}\)?[ .-][0-9]{3}[ .-][0-9]{4}`),
		},
		verify: verifyPhone,
	},
	PersonalDataCard: {
		id:     PersonalDataCard,
		res:    []*regexp.Regexp{regexp.MustCompile(`[0-9](?:[ -]?[0-9]){12,18}`)},
		verify: verifyCard,
	},
	PersonalDataIBAN: {
		id:     PersonalDataIBAN,
		res:    []*regexp.Regexp{regexp.MustCompile(`[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]{1,4}){2,9}`)},
		verify: verifyIBAN,
	},
}

type personalDataDetector struct {
	rules []personalRule
}

// newPersonalDataDetector selects the whole pack, or the ids listed in `rules`.
func newPersonalDataDetector(obj map[string]any) (detectorImpl, error) {
	want, err := stringList(obj, "rules")
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		want = PersonalDataRuleIDs()
	}
	d := &personalDataDetector{}
	seen := map[string]bool{}
	for _, id := range want {
		r, ok := personalRules[id]
		if !ok {
			return nil, fmt.Errorf("rules names %q, which is not a personal data rule id (%s)", id, strings.Join(PersonalDataRuleIDs(), ", "))
		}
		if !seen[id] {
			seen[id] = true
			d.rules = append(d.rules, r)
		}
	}
	return d, nil
}

// segmentLocal: no rule matches across a line break, so a match never crosses
// a message boundary.
func (*personalDataDetector) segmentLocal() {}

func (d *personalDataDetector) detect(_ context.Context, _ Env, _ string, content string) ([]Finding, error) {
	var out []Finding
	for _, r := range d.rules {
		var own []Finding
		for _, re := range r.res {
			for _, m := range re.FindAllStringIndex(content, -1) {
				start, end, ok := r.verify(content, m[0], m[1])
				if !ok || overlaps(own, start, end) {
					continue
				}
				own = append(own, Finding{Start: start, End: end, Label: r.id, Score: 1})
			}
		}
		out = append(out, own...)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// shared helpers

// overlaps reports whether [start, end) overlaps a finding already made by the
// same rule: the international and the North American phone patterns can both
// read one number.
func overlaps(found []Finding, start, end int) bool {
	for _, f := range found {
		if start < f.End && f.Start < end {
			return true
		}
	}
	return false
}

func isDigit(b byte) bool  { return b >= '0' && b <= '9' }
func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func digitsOf(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// e-mail

// fileTLDs are endings that make `name@2x.png` an asset name, not an address.
var fileTLDs = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "svg": true, "webp": true, "ico": true, "bmp": true,
	"css": true, "js": true, "ts": true, "map": true, "json": true, "html": true, "htm": true, "xml": true,
	"pdf": true, "txt": true, "md": true, "zip": true, "gz": true, "tar": true, "woff": true, "woff2": true,
	"ttf": true, "mp3": true, "mp4": true, "mov": true, "webm": true, "yml": true, "yaml": true, "go": true,
	"py": true, "rs": true, "sh": true, "jsx": true, "tsx": true, "lock": true,
}

// reservedMailDomains are documentation domains (RFC 2606, RFC 6761): a
// placeholder, not a person.
var reservedMailDomains = map[string]bool{
	"example.com": true, "example.org": true, "example.net": true, "example.edu": true,
	"localhost": true, "invalid": true,
}

func verifyEmail(content string, start, end int) (int, int, bool) {
	addr := content[start:end]
	at := strings.LastIndexByte(addr, '@')
	local, domain := addr[:at], strings.ToLower(addr[at+1:])
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return 0, 0, false
	}
	// A trailing dot of the sentence is not part of the domain: the regexp
	// ends on a letter, so it never takes one.
	dot := strings.LastIndexByte(domain, '.')
	if dot < 0 {
		return 0, 0, false
	}
	if fileTLDs[domain[dot+1:]] || reservedMailDomains[domain] {
		return 0, 0, false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return 0, 0, false
		}
	}
	return start, end, true
}

// ---------------------------------------------------------------------------
// phone

func verifyPhone(content string, start, end int) (int, int, bool) {
	// A number does not start inside a longer token.
	if start > 0 && (isDigit(content[start-1]) || isLetter(content[start-1])) {
		return 0, 0, false
	}
	if end < len(content) && isDigit(content[end]) {
		return 0, 0, false
	}
	text := content[start:end]
	digits := digitsOf(text)
	if text[0] == '+' {
		// E.164: country code plus subscriber number is 8 to 15 digits. A
		// shorter run is a version or an arithmetic expression.
		if len(digits) < 8 || len(digits) > 15 {
			return 0, 0, false
		}
		if digits[0] == '0' {
			return 0, 0, false
		}
		// An unseparated "+12345678" is a signed number as often as a phone
		// number: require a separator or a parenthesis group, except for a
		// full-length number of 10 or more digits.
		if len(digits) < 10 && !strings.ContainsAny(text, " ().-") {
			return 0, 0, false
		}
		return start, end, true
	}
	// North American: exactly 10 digits, area code and exchange start 2-9.
	if len(digits) != 10 || digits[3] < '2' {
		return 0, 0, false
	}
	return start, end, true
}

// ---------------------------------------------------------------------------
// payment card

// cardIssuer reports whether the number has a known issuer prefix and a
// length that issuer uses.
func cardIssuer(d string) bool {
	n := len(d)
	p2, p3, p4 := d[:2], d[:3], d[:4]
	switch {
	case d[0] == '4': // Visa
		return n == 13 || n == 16 || n == 19
	case p2 == "34" || p2 == "37": // American Express
		return n == 15
	case p2 >= "51" && p2 <= "55", p4 >= "2221" && p4 <= "2720": // Mastercard
		return n == 16
	case p4 == "6011", p2 == "65", p3 >= "644" && p3 <= "649": // Discover
		return n >= 16 && n <= 19
	case p4 >= "3528" && p4 <= "3589": // JCB
		return n >= 16 && n <= 19
	case p3 >= "300" && p3 <= "305", p2 == "36", p2 == "38", p2 == "39": // Diners Club
		return n >= 14 && n <= 19
	case p2 == "62": // UnionPay
		return n >= 16 && n <= 19
	case p2 == "50", p2 >= "56" && p2 <= "58", p2 == "67": // Maestro
		return n >= 12 && n <= 19
	}
	return false
}

func luhnValid(d string) bool {
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		v := int(d[i] - '0')
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

// cardSeparators reports whether the candidate uses at most one kind of
// separator, and never a double one.
func cardSeparators(text string) bool {
	var sep byte
	for i := 0; i < len(text); i++ {
		if isDigit(text[i]) {
			continue
		}
		if sep == 0 {
			sep = text[i]
		}
		if text[i] != sep {
			return false
		}
	}
	return true
}

func allSameDigit(d string) bool {
	for i := 1; i < len(d); i++ {
		if d[i] != d[0] {
			return false
		}
	}
	return true
}

// verifyCard takes the longest prefix of the candidate that is a card number:
// the regexp is greedy, so "4111 1111 1111 1111 12" must still find the card.
func verifyCard(content string, start, end int) (int, int, bool) {
	if start > 0 {
		b := content[start-1]
		if isDigit(b) || isLetter(b) || b == '_' || b == '-' && start > 1 && isDigit(content[start-2]) {
			return 0, 0, false
		}
	}
	text := content[start:end]
	for stop := len(text); stop >= 13; stop-- {
		if !isDigit(text[stop-1]) {
			continue
		}
		part := text[:stop]
		d := digitsOf(part)
		if len(d) < 13 || len(d) > 19 {
			continue
		}
		if stop < len(text) && isDigit(text[stop]) {
			continue // cutting inside a digit run
		}
		if !cardSeparators(part) || allSameDigit(d) || !cardIssuer(d) || !luhnValid(d) {
			continue
		}
		return start, start + stop, true
	}
	return 0, 0, false
}

// ---------------------------------------------------------------------------
// IBAN

// ibanLengths is the registered IBAN length per country (SWIFT IBAN
// registry). A country that is not listed has no IBAN rule here.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22, "BH": 22, "BR": 29,
	"BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22, "DK": 18, "DO": 28, "EE": 20, "EG": 29,
	"ES": 24, "FI": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28,
	"HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22,
	"MK": 19, "MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15, "PK": 24, "PL": 28, "PS": 29, "PT": 25,
	"QA": 29, "RO": 24, "RS": 22, "SA": 24, "SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27,
	"ST": 25, "SV": 28, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
}

// ibanMod97 reports whether the IBAN (no spaces, upper case) has a valid
// check: move the first four characters to the end, read letters as 10..35,
// and the number mod 97 is 1.
func ibanMod97(iban string) bool {
	rem := 0
	rearranged := iban[4:] + iban[:4]
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		switch {
		case isDigit(c):
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return rem == 1
}

func verifyIBAN(content string, start, end int) (int, int, bool) {
	if start > 0 {
		b := content[start-1]
		if isDigit(b) || isLetter(b) || b == '_' {
			return 0, 0, false
		}
	}
	text := content[start:end]
	length, ok := ibanLengths[text[:2]]
	if !ok {
		return 0, 0, false
	}
	// Walk the candidate and stop after `length` IBAN characters; the regexp
	// is greedy and may have taken the next word.
	count, stop := 0, -1
	for i := 0; i < len(text); i++ {
		if text[i] == ' ' {
			continue
		}
		count++
		if count == length {
			stop = i + 1
			break
		}
	}
	if stop < 0 {
		return 0, 0, false
	}
	if stop < len(text) && text[stop] != ' ' {
		return 0, 0, false // the IBAN would end inside a group
	}
	if end == start+stop && end < len(content) && (isDigit(content[end]) || isLetter(content[end])) {
		return 0, 0, false
	}
	compact := strings.ReplaceAll(text[:stop], " ", "")
	if !ibanMod97(compact) {
		return 0, 0, false
	}
	return start, start + stop, true
}
