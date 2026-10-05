package centrysecrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
)

const maxRewrittenVaultPlaintextBytes = 8 * 1024 * 1024

var ErrInvalidMutation = errors.New("centrysecrets: invalid vault mutation")

type SecretCollection uint8

const (
	RegularSecrets SecretCollection = iota + 1
	HiddenSecrets
)

// Mutation is one exact-name update to the current encrypted vault shape.
// Delete ignores Value. Callers must keep Value out of logs and traces.
type Mutation struct {
	Collection   SecretCollection
	Name         string
	Value        string
	IntegerValue *int64
	Delete       bool
}

// RewriteUnwrapped applies mutations and returns a new Python-compatible
// Fernet token. It has no database, filesystem, environment, or logging side
// effects; durable compare-and-lock behavior belongs to the storage adapter.
func RewriteUnwrapped(storedProjectKey, encryptedVault []byte, mutations []Mutation) ([]byte, error) {
	projectKey, ok := decodeFernetKey(storedProjectKey)
	if !ok {
		return nil, ErrInvalidProjectKey
	}
	defer clearKey(&projectKey)
	return rewrite(projectKey, encryptedVault, mutations, time.Now, rand.Reader)
}

// RewriteWrapped is RewriteUnwrapped for a project key encrypted with the
// explicitly supplied current master Fernet key.
func RewriteWrapped(masterKey, encryptedProjectKey, encryptedVault []byte, mutations []Mutation) ([]byte, error) {
	decodedMasterKey, ok := decodeFernetKey(masterKey)
	if !ok {
		return nil, ErrInvalidMasterKey
	}
	defer clearKey(&decodedMasterKey)

	storedProjectKey, ok := decryptFernet(decodedMasterKey, encryptedProjectKey)
	if !ok {
		return nil, ErrInvalidProjectKey
	}
	defer clearBytes(storedProjectKey)

	projectKey, ok := decodeFernetKey(storedProjectKey)
	if !ok {
		return nil, ErrInvalidProjectKey
	}
	defer clearKey(&projectKey)
	return rewrite(projectKey, encryptedVault, mutations, time.Now, rand.Reader)
}

type mutableStoredVault struct {
	// The current Python vault stores JSON values, not only strings. Model
	// default project IDs are numbers, for example. Preserve untouched values
	// byte-for-byte at the JSON-value level while this mutator writes new secret
	// values as JSON strings.
	Secrets        map[string]json.RawMessage `json:"secrets"`
	HiddenSecrets  map[string]json.RawMessage `json:"hidden_secrets"`
	ExternalAccess map[string]bool            `json:"external_access,omitempty"`
}

func rewrite(
	projectKey [fernetKeyBytes]byte,
	encryptedVault []byte,
	mutations []Mutation,
	now func() time.Time,
	entropy io.Reader,
) ([]byte, error) {
	if !validMutations(mutations) || now == nil || entropy == nil {
		return nil, ErrInvalidMutation
	}

	plaintext, ok := decryptFernet(projectKey, encryptedVault)
	if !ok {
		return nil, ErrInvalidVault
	}
	defer clearBytes(plaintext)
	if len(plaintext) > maxRewrittenVaultPlaintextBytes {
		return nil, ErrInvalidVault
	}

	var stored mutableStoredVault
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil || expectJSONEnd(decoder) != nil || stored.Secrets == nil || stored.HiddenSecrets == nil {
		clearMutableStoredVault(&stored)
		return nil, ErrInvalidVault
	}
	defer clearMutableStoredVault(&stored)

	for _, mutation := range mutations {
		target := stored.Secrets
		if mutation.Collection == HiddenSecrets {
			target = stored.HiddenSecrets
		}
		if mutation.Delete {
			delete(target, mutation.Name)
			continue
		}
		var encoded []byte
		if mutation.IntegerValue != nil {
			encoded = strconv.AppendInt(nil, *mutation.IntegerValue, 10)
		} else {
			var err error
			encoded, err = json.Marshal(mutation.Value)
			if err != nil {
				return nil, ErrInvalidMutation
			}
		}
		target[mutation.Name] = encoded
	}

	updated, err := json.Marshal(stored)
	if err != nil || len(updated) > maxRewrittenVaultPlaintextBytes {
		clearBytes(updated)
		return nil, ErrInvalidVault
	}
	defer clearBytes(updated)
	return encryptFernet(projectKey, updated, now().UTC(), entropy)
}

func validMutations(mutations []Mutation) bool {
	if len(mutations) == 0 {
		return false
	}
	type mutationIdentity struct {
		collection SecretCollection
		name       string
	}
	seen := make(map[mutationIdentity]struct{}, len(mutations))
	for _, mutation := range mutations {
		if mutation.Collection != RegularSecrets && mutation.Collection != HiddenSecrets {
			return false
		}
		if !validSecretName(mutation.Name) {
			return false
		}
		if mutation.IntegerValue != nil && mutation.Value != "" {
			return false
		}
		identity := mutationIdentity{collection: mutation.Collection, name: mutation.Name}
		if _, duplicate := seen[identity]; duplicate {
			return false
		}
		seen[identity] = struct{}{}
	}
	return true
}

func validSecretName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' {
			continue
		}
		return false
	}
	return true
}

func encryptFernet(key [fernetKeyBytes]byte, plaintext []byte, issuedAt time.Time, entropy io.Reader) ([]byte, error) {
	if len(plaintext) == 0 || entropy == nil {
		return nil, ErrInvalidVault
	}
	padded := addPKCS7Padding(plaintext)
	defer clearBytes(padded)

	block, err := aes.NewCipher(key[fernetKeyBytes/2:])
	if err != nil {
		return nil, ErrInvalidVault
	}
	raw := make([]byte, fernetHeaderBytes+len(padded)+fernetMACBytes)
	raw[0] = fernetVersion
	binary.BigEndian.PutUint64(raw[1:9], uint64(issuedAt.Unix()))
	iv := raw[9:fernetHeaderBytes]
	if _, err := io.ReadFull(entropy, iv); err != nil {
		clearBytes(raw)
		return nil, ErrInvalidVault
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(raw[fernetHeaderBytes:len(raw)-fernetMACBytes], padded)

	mac := hmac.New(sha256.New, key[:fernetKeyBytes/2])
	_, _ = mac.Write(raw[:len(raw)-fernetMACBytes])
	mac.Sum(raw[len(raw)-fernetMACBytes:][:0])

	encoded := make([]byte, base64.URLEncoding.EncodedLen(len(raw)))
	base64.URLEncoding.Encode(encoded, raw)
	clearBytes(raw)
	return encoded, nil
}

func addPKCS7Padding(plaintext []byte) []byte {
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	result := make([]byte, len(plaintext)+padding)
	copy(result, plaintext)
	for index := len(plaintext); index < len(result); index++ {
		result[index] = byte(padding)
	}
	return result
}

func clearMutableStoredVault(stored *mutableStoredVault) {
	if stored == nil {
		return
	}
	for name, value := range stored.Secrets {
		clearBytes(value)
		delete(stored.Secrets, name)
	}
	for name := range stored.ExternalAccess {
		delete(stored.ExternalAccess, name)
	}
	for name, value := range stored.HiddenSecrets {
		clearBytes(value)
		delete(stored.HiddenSecrets, name)
	}
}
