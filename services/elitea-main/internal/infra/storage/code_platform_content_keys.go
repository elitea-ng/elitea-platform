package storage

import (
	"context"
	"encoding/base64"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

const CodePlatformContentKeyFileLimit = 4096

// CodePlatformContentKeyring is immutable startup material owned only by Main.
// At most eight retired read keys accompany the current write key.
type CodePlatformContentKeyring struct {
	current string
	keys    map[string][32]byte
}

func codeContentKeyID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
func ParseCodePlatformContentKeys(raw []byte) (*CodePlatformContentKeyring, error) {
	var wire struct {
		Revision     uint8  `json:"revision"`
		CurrentKeyID string `json:"current_key_id"`
		Keys         []struct {
			ID  string `json:"id"`
			Key string `json:"key_base64url"`
		} `json:"keys"`
	}
	if len(raw) < 1 || len(raw) > CodePlatformContentKeyFileLimit || code.Decode(raw, &wire, CodePlatformContentKeyFileLimit) != nil || wire.Revision != 1 || !codeContentKeyID(wire.CurrentKeyID) || len(wire.Keys) < 1 || len(wire.Keys) > 9 {
		return nil, domain.ErrUnavailable
	}
	result := &CodePlatformContentKeyring{current: wire.CurrentKeyID, keys: make(map[string][32]byte, len(wire.Keys))}
	for _, entry := range wire.Keys {
		if !codeContentKeyID(entry.ID) || len(entry.Key) != 43 {
			return nil, domain.ErrUnavailable
		}
		if _, exists := result.keys[entry.ID]; exists {
			return nil, domain.ErrUnavailable
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(entry.Key)
		if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != entry.Key {
			clearContentBytes(decoded)
			return nil, domain.ErrUnavailable
		}
		var material [32]byte
		copy(material[:], decoded)
		clearContentBytes(decoded)
		if material == [32]byte{} {
			return nil, domain.ErrUnavailable
		}
		for _, existing := range result.keys {
			if existing == material {
				clearContentBytes(material[:])
				return nil, domain.ErrUnavailable
			}
		}
		result.keys[entry.ID] = material
		clearContentBytes(material[:])
	}
	if _, exists := result.keys[result.current]; !exists {
		return nil, domain.ErrUnavailable
	}
	return result, nil
}
func (k *CodePlatformContentKeyring) CurrentCodePlatformKey(ctx context.Context) (string, [32]byte, error) {
	if k == nil || ctx == nil || ctx.Err() != nil {
		return "", [32]byte{}, domain.ErrUnavailable
	}
	key, exists := k.keys[k.current]
	if !exists {
		return "", [32]byte{}, domain.ErrUnavailable
	}
	return k.current, key, nil
}
func (k *CodePlatformContentKeyring) ResolveCodePlatformKey(ctx context.Context, id string) ([32]byte, error) {
	if k == nil || ctx == nil || ctx.Err() != nil || !codeContentKeyID(id) {
		return [32]byte{}, domain.ErrUnavailable
	}
	key, exists := k.keys[id]
	if !exists {
		return [32]byte{}, domain.ErrUnavailable
	}
	return key, nil
}
