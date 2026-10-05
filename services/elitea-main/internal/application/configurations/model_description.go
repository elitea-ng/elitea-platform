package configurations

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LLMModelDescriptionField is the optional `data` field of an llm_model
// configuration that says, in a few words, what the model is good for
// ("Fast for everyday tasks"). The model pickers show it as a second line
// under the model name.
//
// It is display text only. Nothing sends it to the gateway or to a provider,
// and it does not change which model a request uses.
const LLMModelDescriptionField = "description"

// MaxLLMModelDescriptionRunes is the longest description, in characters. The
// model menu shows 40 characters on one line without cutting them off.
const MaxLLMModelDescriptionRunes = 40

// ErrLLMModelDescriptionInvalid is the refusal for a description that is not
// a string or is longer than MaxLLMModelDescriptionRunes.
var ErrLLMModelDescriptionInvalid = errors.New("description must be text of at most 40 characters")

// ErrLLMModelDescriptionHiddenCharacters is the refusal for a description
// that holds a control or format character (Unicode categories Cc and Cf).
//
// Every member of the project sees the description in the model picker, and
// every user sees it for a platform model. A bidi override (U+202A to U+202E,
// U+2066 to U+2069) or a zero-width character (U+200B to U+200F) changes what
// the text looks like without changing what it is, so an editor could label a
// model to read like another one. React escapes the text, so this is not a
// script injection; it is a spoofing of the label.
var ErrLLMModelDescriptionHiddenCharacters = errors.New(
	"description must not contain control, text-direction or zero-width characters")

// hasHiddenCharacters reports a rune of Unicode category Cc or Cf.
func hasHiddenCharacters(text string) bool {
	return strings.IndexFunc(text, isHiddenCharacter) >= 0
}

func isHiddenCharacter(r rune) bool {
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}

// NormalizeLLMModelDescription applies the description rules to an llm_model
// `data` object, in place:
//
//   - leading and trailing spaces are removed;
//   - an empty value, a value of spaces only and a JSON null remove the key,
//     so a cleared description is not stored as "";
//   - a value that is not a string, or is longer than 40 characters after the
//     trim, is refused with ErrLLMModelDescriptionInvalid;
//   - a value that holds a control or format character (bidi controls,
//     zero-width characters) is refused with
//     ErrLLMModelDescriptionHiddenCharacters.
//
// A nil map and a map without the key are valid and unchanged.
func NormalizeLLMModelDescription(data map[string]any) error {
	raw, present := data[LLMModelDescriptionField]
	if !present {
		return nil
	}
	if raw == nil {
		delete(data, LLMModelDescriptionField)
		return nil
	}
	text, ok := raw.(string)
	if !ok {
		return ErrLLMModelDescriptionInvalid
	}
	text = strings.TrimSpace(text)
	if text == "" {
		delete(data, LLMModelDescriptionField)
		return nil
	}
	if utf8.RuneCountInString(text) > MaxLLMModelDescriptionRunes {
		return ErrLLMModelDescriptionInvalid
	}
	if hasHiddenCharacters(text) {
		return ErrLLMModelDescriptionHiddenCharacters
	}
	data[LLMModelDescriptionField] = text
	return nil
}

// ReadLLMModelDescription reads a STORED description for the model catalogue.
// It is tolerant on purpose: a row written before the rule existed, or
// written by a path that skipped it, keeps its place in the picker. A value
// that is not a string is ignored, control and format characters are removed
// (see ErrLLMModelDescriptionHiddenCharacters), and a long value is cut to
// the limit.
func ReadLLMModelDescription(raw any) *string {
	text, ok := raw.(string)
	if !ok {
		return nil
	}
	if hasHiddenCharacters(text) {
		text = strings.Map(func(r rune) rune {
			if isHiddenCharacter(r) {
				return -1
			}
			return r
		}, text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) > MaxLLMModelDescriptionRunes {
		text = strings.TrimSpace(string([]rune(text)[:MaxLLMModelDescriptionRunes]))
	}
	return &text
}

// llmModelDescriptionInfo is the (i) text of the form field.
const llmModelDescriptionInfo = "A few words on what the model is best for, shown under its name when " +
	"people pick a model, for example Fast for everyday tasks or Best for coding and agents."

// addLLMModelFormContract adds the replatform's llm_model form contract to the
// pinned legacy schema, without a change to the snapshot file itself:
//
//   - the optional `description` field, with its 40-character limit;
//   - has_test_connection, because POST /configurations/check_connection
//     now tests an llm_model with one real completion (the check_connection.go
//     file of the configurations API package).
func (catalog *CurrentAvailableCatalog) addLLMModelFormContract() error {
	index, ok := catalog.entryIndexes["llm_model"]
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	var schema map[string]any
	if err := json.Unmarshal(catalog.entries[index].ConfigSchema, &schema); err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	data, ok := properties["data"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	fields, ok := data["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	fields[LLMModelDescriptionField] = map[string]any{
		"title":       "Description",
		"description": llmModelDescriptionInfo,
		"default":     nil,
		"anyOf": []any{
			map[string]any{"type": "string", "maxLength": MaxLLMModelDescriptionRunes},
			map[string]any{"type": "null"},
		},
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entries[index].ConfigSchema = encoded
	catalog.entries[index].HasTestConnection = true
	return nil
}
