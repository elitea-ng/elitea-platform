package credentialadapters

import (
	"fmt"
	"strings"
)

// Qualification values (gate D6). No provider × class is supported until a
// qualification run links its evidence.
const (
	// QualificationUnqualified: nothing proves the class works.
	QualificationUnqualified = "unqualified"
	// QualificationFixture: proven against a test double only.
	QualificationFixture = "fixture"
	// qualifiedPrefix starts a qualified value; the evidence ref follows.
	qualifiedPrefix = "qualified:"
)

// Qualified is the qualification value for a class proven by the evidence
// at ref (for example an issue or a qualification run id).
func Qualified(ref string) string { return qualifiedPrefix + ref }

// validQualification accepts "unqualified", "fixture" and
// "qualified:<ref>" with a non-blank ref.
func validQualification(value string) error {
	switch value {
	case QualificationUnqualified, QualificationFixture:
		return nil
	}
	if ref, ok := strings.CutPrefix(value, qualifiedPrefix); ok {
		if strings.TrimSpace(ref) == "" || strings.ContainsAny(ref, " \t\r\n") {
			return fmt.Errorf("%q names no evidence ref", value)
		}
		return nil
	}
	return fmt.Errorf("%q is not unqualified, fixture or qualified:<evidence-ref>", value)
}
