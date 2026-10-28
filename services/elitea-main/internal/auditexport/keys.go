package auditexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"
)

const (
	// MaxSigningKeys is how many keys the key file may hold: the current key
	// and the one being retired (or introduced).
	MaxSigningKeys = 2
	// MinSecretBytes is the shortest accepted HMAC secret.
	MinSecretBytes = 32
	maxKeyFileSize = 16 << 10
	// keyReloadInterval is how often the key file is read again.
	keyReloadInterval = 30 * time.Second
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// keyFile is the JSON key file:
//
//	{"keys":[{"id":"2026-10","secret":"<32+ chars>","created":"2026-10-01T00:00:00Z"}]}
//
// The key with the latest "created" signs; a second key may be listed while a
// rotation is in progress so a rollback is one edit. A receiver verifies by
// the X-Elitea-Key-Id header and so can hold both.
type keyFile struct {
	Keys []struct {
		ID      string `json:"id"`
		Secret  string `json:"secret"`
		Created string `json:"created"`
	} `json:"keys"`
}

// ParseKeyFile validates the key file's bytes. Errors name the field, never
// the secret.
func ParseKeyFile(raw []byte) ([]SigningKey, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var file keyFile
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("webhook key file is not the expected JSON document")
	}
	if len(file.Keys) == 0 || len(file.Keys) > MaxSigningKeys {
		return nil, fmt.Errorf("webhook key file must list 1 to %d keys", MaxSigningKeys)
	}
	seen := map[string]bool{}
	keys := make([]SigningKey, 0, len(file.Keys))
	for i, k := range file.Keys {
		if !keyIDPattern.MatchString(k.ID) {
			return nil, fmt.Errorf("webhook key %d: id must match %s", i, keyIDPattern)
		}
		if seen[k.ID] {
			return nil, fmt.Errorf("webhook key %d: duplicate id", i)
		}
		seen[k.ID] = true
		if len(k.Secret) < MinSecretBytes {
			return nil, fmt.Errorf("webhook key %q: secret must be at least %d bytes", k.ID, MinSecretBytes)
		}
		created, err := time.Parse(time.RFC3339, k.Created)
		if err != nil {
			return nil, fmt.Errorf("webhook key %q: created must be an RFC 3339 time", k.ID)
		}
		keys = append(keys, SigningKey{ID: k.ID, Secret: []byte(k.Secret), Created: created})
	}
	if len(keys) == 2 && keys[0].Created.Equal(keys[1].Created) {
		return nil, errors.New("webhook key file: two keys have the same created time, so the newest is ambiguous")
	}
	return keys, nil
}

// FileKeySource reads path now, to fail fast, and then at most every
// keyReloadInterval. A later read error keeps the last good keys, so a
// secret volume that is briefly unavailable does not stop delivery.
func FileKeySource(path string, now func() time.Time) (KeySource, error) {
	if now == nil {
		now = time.Now
	}
	load := func() ([]SigningKey, error) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxKeyFileSize {
			return nil, errors.New("webhook key file is too large")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return ParseKeyFile(raw)
	}
	keys, err := load()
	if err != nil {
		return nil, fmt.Errorf("read webhook key file: %w", err)
	}
	var (
		mu     sync.Mutex
		cached = keys
		loaded = now()
	)
	return func() ([]SigningKey, error) {
		mu.Lock()
		defer mu.Unlock()
		if now().Sub(loaded) < keyReloadInterval {
			return cached, nil
		}
		loaded = now()
		fresh, err := load()
		if err != nil {
			return cached, nil
		}
		cached = fresh
		return cached, nil
	}, nil
}
