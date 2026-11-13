// Package entitylimits holds the one pair of length limits every
// user-editable entity name and description obeys (issue 1968), so the
// handlers, the contract (api/openapi/v2.yaml maxLength), the migration CHECK
// constraints and the clients (libs/ts/client-core) state the same numbers.
//
// Lengths count characters (runes), never bytes. Chat message bodies, agent
// instructions and welcome messages are NOT covered: they stay unlimited.
package entitylimits

import (
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	// MaxNameLength is the longest name an entity (agent, pipeline, toolkit,
	// configuration, token, folder, conversation, skill) can have.
	MaxNameLength = 128
	// MaxDescriptionLength is the longest description an entity can have.
	MaxDescriptionLength = 2000
)

// CheckName refuses a name longer than MaxNameLength characters with a 400
// naming field.
func CheckName(field, value string) error {
	return check(field, value, MaxNameLength)
}

// CheckDescription refuses a description longer than MaxDescriptionLength
// characters with a 400 naming field.
func CheckDescription(field, value string) error {
	return check(field, value, MaxDescriptionLength)
}

// Truncate cuts value to max characters (never inside a rune). It is for text
// the platform itself produces (a conversation named from a first message),
// never for client input, which is refused instead.
func Truncate(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}

func check(field, value string, max int) error {
	if utf8.RuneCountInString(value) > max {
		return apierr.FieldTooLong(field, max)
	}
	return nil
}

// CheckBody checks the `name` and `description` keys of a free-form JSON body,
// the shape most create/update handlers decode into. Absent or non-string keys
// are left for the handler's own validation.
func CheckBody(body map[string]any) error {
	if name, ok := body["name"].(string); ok {
		if err := CheckName("name", name); err != nil {
			return err
		}
	}
	if description, ok := body["description"].(string); ok {
		return CheckDescription("description", description)
	}
	return nil
}
