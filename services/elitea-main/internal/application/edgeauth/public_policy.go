package edgeauth

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxPublicRules       = 256
	MaxConditionsPerRule = 7
	MaxRuleNameBytes     = 128
	MaxPatternBytes      = 2048
)

type SourceField uint8

const (
	SourceMethod SourceField = iota + 1
	SourceProto
	SourceHost
	SourceURI
	SourceIP
	SourceTarget
	SourceScope
)

type RuleCondition struct {
	Field   SourceField
	Pattern string
}

type PublicRule struct {
	Name string
	// MatchAll must be explicit because a zero-condition Python rule silently
	// authorizes every request. It is mutually exclusive with Conditions.
	MatchAll   bool
	Conditions []RuleCondition
}

type compiledCondition struct {
	field   SourceField
	pattern *regexp.Regexp
}

type compiledRule struct {
	name       string
	conditions []compiledCondition
}

// PublicPolicy is immutable after construction. It compiles only bounded
// configuration regexes and never caches a request or authorization decision.
type PublicPolicy struct {
	rules []compiledRule
}

func NewPublicPolicy(rules []PublicRule) (PublicPolicy, error) {
	if len(rules) > MaxPublicRules {
		return PublicPolicy{}, ErrInvalidConfiguration
	}

	compiled := make([]compiledRule, 0, len(rules))
	names := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if rule.Name == "" || rule.Name != strings.TrimSpace(rule.Name) ||
			len(rule.Name) > MaxRuleNameBytes || !utf8.ValidString(rule.Name) ||
			stringsContainControl(rule.Name) || len(rule.Conditions) > MaxConditionsPerRule ||
			(rule.MatchAll && len(rule.Conditions) != 0) || (!rule.MatchAll && len(rule.Conditions) == 0) {
			return PublicPolicy{}, ErrInvalidConfiguration
		}
		if _, duplicate := names[rule.Name]; duplicate {
			return PublicPolicy{}, ErrInvalidConfiguration
		}
		names[rule.Name] = struct{}{}
		seen := make(map[SourceField]struct{}, len(rule.Conditions))
		conditions := make([]compiledCondition, 0, len(rule.Conditions))
		for _, condition := range rule.Conditions {
			if !validSourceField(condition.Field) || len(condition.Pattern) > MaxPatternBytes ||
				!utf8.ValidString(condition.Pattern) {
				return PublicPolicy{}, ErrInvalidConfiguration
			}
			if _, duplicate := seen[condition.Field]; duplicate {
				return PublicPolicy{}, ErrInvalidConfiguration
			}
			seen[condition.Field] = struct{}{}
			pattern, err := regexp.Compile(normalizeCurrentPattern(condition.Pattern))
			if err != nil {
				return PublicPolicy{}, fmt.Errorf("%w: public rule pattern", ErrInvalidConfiguration)
			}
			conditions = append(conditions, compiledCondition{
				field:   condition.Field,
				pattern: pattern,
			})
		}
		compiled = append(compiled, compiledRule{name: rule.Name, conditions: conditions})
	}
	return PublicPolicy{rules: compiled}, nil
}

// normalizeCurrentPattern preserves the redundant escaped hyphen used by the
// tracked Python public-route configuration. Go's RE2 parser rejects \- while
// Python accepts it. Outside a character class the escape is redundant; inside
// a class, \x2d preserves a literal hyphen without creating a range.
func normalizeCurrentPattern(pattern string) string {
	var normalized []byte
	inClass := false
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		if character == '\\' && index+1 < len(pattern) {
			next := pattern[index+1]
			if next == '-' {
				if normalized == nil {
					normalized = make([]byte, 0, len(pattern)+3)
					normalized = append(normalized, pattern[:index]...)
				}
				if inClass {
					normalized = append(normalized, `\x2d`...)
				} else {
					normalized = append(normalized, '-')
				}
				index++
				continue
			}
			if normalized != nil {
				normalized = append(normalized, character, next)
			}
			index++
			continue
		}
		switch character {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		if normalized != nil {
			normalized = append(normalized, character)
		}
	}
	if normalized == nil {
		return pattern
	}
	return string(normalized)
}

func (p PublicPolicy) match(source Source) (PublicMatch, bool) {
	for index, rule := range p.rules {
		if rule.matches(source) {
			return PublicMatch{RuleIndex: index, RuleName: rule.name}, true
		}
	}
	return PublicMatch{}, false
}

func (r compiledRule) matches(source Source) bool {
	for _, condition := range r.conditions {
		value, present := sourceValue(source, condition.field)
		if !present {
			return false
		}
		location := condition.pattern.FindStringIndex(value)
		if location == nil || location[0] != 0 || location[1] != len(value) {
			return false
		}
	}
	return true
}

func sourceValue(source Source, field SourceField) (string, bool) {
	switch field {
	case SourceMethod:
		return source.Method, true
	case SourceProto:
		return source.Proto, true
	case SourceHost:
		return source.Host, true
	case SourceURI:
		return source.URI, true
	case SourceIP:
		return source.IP, true
	case SourceTarget:
		return source.Target, source.TargetPresent
	case SourceScope:
		return source.Scope, source.ScopePresent
	default:
		return "", false
	}
}

func validSourceField(field SourceField) bool {
	return field >= SourceMethod && field <= SourceScope
}

func stringsContainControl(value string) bool {
	return containsRune(value, unicode.IsControl)
}

func containsRune(value string, predicate func(rune) bool) bool {
	for _, character := range value {
		if predicate(character) {
			return true
		}
	}
	return false
}
