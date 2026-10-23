// Package session defines the server-side browser authentication state shared
// by identity providers and the HTTP authentication boundary.
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

const (
	CurrentSchemaVersion         = 1
	MaxErrorBytes                = 1 << 10
	MaxProviderBytes             = 128
	MaxProviderAttributesBytes   = 32 << 10
	MaxProviderAttributesNesting = 128
)

var (
	ErrInvalidState = errors.New("invalid browser session state")
	ErrNotFound     = errors.New("browser session not found")
)

// State is the language-neutral replacement for the legacy auth plugin's auth
// context persisted in the server-side session. ProviderAttributes is a JSON
// object so provider-specific claims remain at the authentication boundary
// instead of becoming untyped fields throughout the service. This preserves
// the current-baseline provider_attr object, including its nested sessionindex
// value.
type State struct {
	SchemaVersion      int             `json:"schema_version"`
	Done               bool            `json:"done"`
	Error              string          `json:"error"`
	Expiration         *time.Time      `json:"expiration"`
	Provider           *string         `json:"provider"`
	ProviderAttributes json.RawMessage `json:"provider_attr"`
	UserID             *int64          `json:"user_id"`
}

// Validate checks the fixed session schema and its allocation bounds. It does
// not decide whether Expiration has elapsed; authorization owns that clock
// check while storage retains the state for logout until the session TTL.
func (s State) Validate() error {
	if s.SchemaVersion != CurrentSchemaVersion {
		return invalid("unsupported schema version")
	}
	if len(s.Error) > MaxErrorBytes || !utf8.ValidString(s.Error) {
		return invalid("error value is invalid")
	}
	if s.Provider != nil && (len(*s.Provider) > MaxProviderBytes || !utf8.ValidString(*s.Provider)) {
		return invalid("provider value is invalid")
	}
	if s.Expiration != nil && s.Expiration.IsZero() {
		return invalid("expiration is invalid")
	}
	if s.UserID != nil && *s.UserID <= 0 {
		return invalid("user ID is invalid")
	}
	if len(s.ProviderAttributes) == 0 || len(s.ProviderAttributes) > MaxProviderAttributesBytes ||
		!utf8.Valid(s.ProviderAttributes) {
		return invalid("provider attributes size is invalid")
	}
	if err := validateUniqueJSONObject(s.ProviderAttributes); err != nil {
		return invalid("provider attributes must be a JSON object")
	}
	return nil
}

// validateUniqueJSONObject rejects duplicate member names at every nesting
// level. JSON permits implementations to choose which duplicate wins, so
// accepting duplicate identity-bearing claims would make Go, Python, and
// future worker/provider consumers disagree about the authenticated identity.
func validateUniqueJSONObject(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return errors.New("root value is not an object")
	}
	if err := consumeUniqueJSONObject(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONObject(decoder *json.Decoder, depth int) error {
	if depth > MaxProviderAttributesNesting {
		return errors.New("JSON nesting exceeds the session limit")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("object member name is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate object member %q", key)
		}
		seen[key] = struct{}{}
		if err := consumeUniqueJSONValue(decoder, depth); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("object is not closed")
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return consumeUniqueJSONObject(decoder, depth+1)
	case '[':
		if depth >= MaxProviderAttributesNesting {
			return errors.New("JSON nesting exceeds the session limit")
		}
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if end, ok := closing.(json.Delim); !ok || end != ']' {
			return errors.New("array is not closed")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidState, reason)
}
