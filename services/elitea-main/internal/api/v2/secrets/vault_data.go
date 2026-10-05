package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// vaultData is the JSON stored (after Fernet encryption) in centry.secrets_data.
//
// THE VALUES ARE JSON VALUES, NOT ONLY STRINGS. The vault format is pylon's,
// and pylon stores whatever Python value it was handed. The Configurations
// default-model write (infra/db/repos.SetCurrentModelDefault) stores
// `default_<section>_model_project_id` as a JSON INTEGER on purpose, and the
// model-default reader (centrysecrets.LookupRegularProjectID) and pylon both
// read it back as one. A `map[string]string` decode refused the whole vault on
// that one integer, so every project route answered 500 "project vault is
// unreadable" for a project whose only fault was having a default model
// selected — the Settings > Secrets page, the e2e cleanup sweep, and every
// create (F1 / UI-PD-1).
//
// The two maps stay `map[string]string`, which is what the routes read and
// write. A non-string value is exposed as its JSON text — `42`, `true`,
// `null`, `{"a":1}` — and its raw bytes are kept beside it. On write-back a
// value whose text is unchanged is written as the raw JSON it was read as, so
// an integer stays an integer. A value a route changed is written as a JSON
// string, which is what every route means by a secret value.
type vaultData struct {
	Secrets       map[string]string
	HiddenSecrets map[string]string

	// raw holds the original JSON of every non-string value, per collection.
	// nil for a vault built in memory.
	raw map[vaultCollection]map[string]json.RawMessage
}

type vaultCollection uint8

const (
	regularCollection vaultCollection = iota + 1
	hiddenCollection
)

// storedVault is the on-disk shape.
type storedVault struct {
	Secrets       map[string]json.RawMessage `json:"secrets"`
	HiddenSecrets map[string]json.RawMessage `json:"hidden_secrets"`
}

// UnmarshalJSON decodes a stored vault, accepting any JSON value per secret.
func (v *vaultData) UnmarshalJSON(data []byte) error {
	var stored storedVault
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	decoded := vaultData{}
	var err error
	if decoded.Secrets, err = decoded.decodeCollection(regularCollection, stored.Secrets); err != nil {
		return err
	}
	if decoded.HiddenSecrets, err = decoded.decodeCollection(hiddenCollection, stored.HiddenSecrets); err != nil {
		return err
	}
	*v = decoded
	return nil
}

func (v *vaultData) decodeCollection(collection vaultCollection, stored map[string]json.RawMessage) (map[string]string, error) {
	if stored == nil {
		return nil, nil
	}
	values := make(map[string]string, len(stored))
	for name, raw := range stored {
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 && raw[0] == '"' {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, fmt.Errorf("decode secret value: %w", err)
			}
			values[name] = value
			continue
		}
		// A number, a boolean, null, or a structure: exposed as its compact
		// JSON text, and remembered so a write-back keeps its type.
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return nil, fmt.Errorf("decode secret value: %w", err)
		}
		if v.raw == nil {
			v.raw = map[vaultCollection]map[string]json.RawMessage{}
		}
		if v.raw[collection] == nil {
			v.raw[collection] = map[string]json.RawMessage{}
		}
		text := compact.String()
		v.raw[collection][name] = json.RawMessage(text)
		values[name] = text
	}
	return values, nil
}

// MarshalJSON encodes the vault in the on-disk shape. A value still equal to
// the JSON text of the non-string value it was read as is written back as
// that value; everything else is a JSON string.
func (v vaultData) MarshalJSON() ([]byte, error) {
	stored := storedVault{
		Secrets:       v.encodeCollection(regularCollection, v.Secrets),
		HiddenSecrets: v.encodeCollection(hiddenCollection, v.HiddenSecrets),
	}
	return json.Marshal(stored)
}

func (v vaultData) encodeCollection(collection vaultCollection, values map[string]string) map[string]json.RawMessage {
	// pylon always writes both keys, as objects; an empty map is `{}`.
	encoded := make(map[string]json.RawMessage, len(values))
	original := v.raw[collection]
	for name, value := range values {
		if raw, ok := original[name]; ok && string(raw) == value {
			encoded[name] = raw
			continue
		}
		quoted, _ := json.Marshal(value) // a Go string always marshals
		encoded[name] = quoted
	}
	return encoded
}

// moveToHidden moves a regular secret into hidden_secrets, carrying its stored
// JSON type with it — pylon's hide moves the Python value as it is.
func (v *vaultData) moveToHidden(name string) bool {
	value, ok := v.Secrets[name]
	if !ok {
		return false
	}
	delete(v.Secrets, name)
	v.HiddenSecrets[name] = value
	if raw, typed := v.raw[regularCollection][name]; typed {
		delete(v.raw[regularCollection], name)
		if v.raw[hiddenCollection] == nil {
			v.raw[hiddenCollection] = map[string]json.RawMessage{}
		}
		v.raw[hiddenCollection][name] = raw
	} else {
		delete(v.raw[hiddenCollection], name)
	}
	return true
}

// rename moves regular secret `oldName` to `newName` holding `value`, and
// carries its stored JSON type across when the value is unchanged. A rename
// alone does not change a value: `42` stays the integer 42 and `false` stays
// the boolean false (the STRING "false" is truthy in pylon, so writing it as
// one would flip its meaning). A changed value is a JSON string, as every
// other route write is.
func (v *vaultData) rename(oldName, newName, value string) {
	raw, typed := v.raw[regularCollection][oldName]
	delete(v.Secrets, oldName)
	delete(v.raw[regularCollection], oldName)
	delete(v.raw[regularCollection], newName)
	v.Secrets[newName] = value
	if typed && string(raw) == value {
		v.raw[regularCollection][newName] = raw // typed ⇒ the inner map exists
	}
}

// credential reads `name` for use AS A CREDENTIAL — regular first, then
// hidden, the order ResolveSecretValue reads them in.
//
// A value stored as anything but a JSON string is NOT a credential. Its text
// (`null`, `true`, `0`) is guessable, so it reports found=true, usable=false
// and the caller must treat it as no value at all. Pylon compares the Python
// value itself, so a stored `None` never equals any header; reading it as the
// text "null" would accept `X-SECRET: null`.
func (v vaultData) credential(name string) (value string, found, usable bool) {
	if text, ok := v.Secrets[name]; ok {
		_, typed := v.raw[regularCollection][name]
		return text, true, !typed
	}
	if text, ok := v.HiddenSecrets[name]; ok {
		_, typed := v.raw[hiddenCollection][name]
		return text, true, !typed
	}
	return "", false, false
}
