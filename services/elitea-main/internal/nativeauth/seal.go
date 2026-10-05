package nativeauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The refresh re-delivery grace window (coordinator decision 7) needs the
// successor pair of a consumed refresh token kept somewhere, and the server
// must not keep a usable plaintext token at rest. The pair is sealed with
// AES-256-GCM under a key DERIVED FROM THE CONSUMED TOKEN ITSELF, which the
// server stores only as a SHA-256 hash under a different derivation. So:
//
//   - a database reader learns nothing: the key is never stored;
//   - only a caller presenting the consumed token can open its successor, and
//     that caller is exactly the one the window is for (a client whose
//     response was lost);
//   - the additional data binds the box to its family and generation, so a box
//     cannot be moved to another row.

const sealKeyLabel = "elitea-native-refresh-redelivery-v1\x00"

type sealedPair struct {
	AccessToken     string `json:"a"`
	RefreshToken    string `json:"r"`
	AccessExpiresAt int64  `json:"x"`
}

func sealKey(presentedRefreshToken string) []byte {
	sum := sha256.Sum256([]byte(sealKeyLabel + presentedRefreshToken))
	return sum[:]
}

func sealAAD(sessionID string, generation int) []byte {
	return []byte(fmt.Sprintf("%s/%d", sessionID, generation))
}

func sealSuccessor(presented, sessionID string, generation int, pair sealedPair) ([]byte, error) {
	plaintext, err := json.Marshal(pair)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sealKey(presented))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, sealAAD(sessionID, generation)), nil
}

var errSealOpen = errors.New("nativeauth: sealed successor does not open")

func openSuccessor(presented, sessionID string, generation int, box []byte) (sealedPair, error) {
	block, err := aes.NewCipher(sealKey(presented))
	if err != nil {
		return sealedPair{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealedPair{}, err
	}
	if len(box) < aead.NonceSize() {
		return sealedPair{}, errSealOpen
	}
	plaintext, err := aead.Open(nil, box[:aead.NonceSize()], box[aead.NonceSize():], sealAAD(sessionID, generation))
	if err != nil {
		return sealedPair{}, errSealOpen
	}
	var pair sealedPair
	if err := json.Unmarshal(plaintext, &pair); err != nil {
		return sealedPair{}, errSealOpen
	}
	return pair, nil
}

func (p sealedPair) accessExpiresAt() time.Time { return time.Unix(p.AccessExpiresAt, 0).UTC() }
