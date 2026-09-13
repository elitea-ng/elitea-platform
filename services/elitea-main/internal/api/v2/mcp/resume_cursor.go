package mcp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const maxResumeCursorBytes = 8192
const resumeCursorLifetime = 24 * time.Hour

var errResumeCursor = errors.New("MCP resume cursor unavailable")

// ResumeCursorCodec uses the deployment key, so cursors survive Main replacement.
// The cursor is a result locator. Authentication and current permissions remain required.
type ResumeCursorCodec struct{ aead cipher.AEAD }

func NewResumeCursorCodec(masterKey []byte) (*ResumeCursorCodec, error) {
	if len(masterKey) != 32 {
		return nil, errResumeCursor
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, "elitea/mcp-response-cursor/v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &ResumeCursorCodec{aead: aead}, nil
}

// Fields contain admission metadata only. No prompt, credential, or result is encoded.
type resumeCursor struct {
	StreamID             string          `json:"s"`
	ProjectID            int64           `json:"p"`
	ActorID              int64           `json:"a"`
	Scope                string          `json:"sc"`
	RequestID            json.RawMessage `json:"r"`
	ExecutionID          string          `json:"e"`
	ResponseMessageID    string          `json:"m"`
	ApplicationID        int64           `json:"app"`
	ApplicationVersionID int64           `json:"v"`
	ToolName             string          `json:"t"`
	ExpiresAt            int64           `json:"x"`
	Complete             bool            `json:"done,omitempty"`
}

func (c resumeCursor) valid(now time.Time) bool {
	return c.StreamID != "" && len(c.StreamID) <= 64 && c.ProjectID > 0 && c.ActorID > 0 &&
		len(c.Scope) <= 256 && len(c.RequestID) > 0 && len(c.RequestID) <= 1024 && json.Valid(c.RequestID) &&
		c.ExecutionID != "" && len(c.ExecutionID) <= 256 && c.ResponseMessageID != "" && len(c.ResponseMessageID) <= 256 &&
		c.ApplicationID > 0 && c.ApplicationVersionID > 0 && c.ToolName != "" && len(c.ToolName) <= 256 &&
		c.ExpiresAt > now.Unix() && c.ExpiresAt <= now.Add(resumeCursorLifetime).Unix()
}

func (c *ResumeCursorCodec) seal(value resumeCursor, now time.Time) (string, error) {
	if c == nil || !value.valid(now) {
		return "", errResumeCursor
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return "", errResumeCursor
	}
	defer clear(plain)
	encoded := base64.RawURLEncoding.EncodeToString(c.aead.Seal(nil, nil, plain, []byte("mcp-response-v1")))
	if len(encoded) > maxResumeCursorBytes {
		return "", errResumeCursor
	}
	return encoded, nil
}

func (c *ResumeCursorCodec) open(encoded string, now time.Time) (resumeCursor, error) {
	if c == nil || len(encoded) == 0 || len(encoded) > maxResumeCursorBytes {
		return resumeCursor{}, errResumeCursor
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return resumeCursor{}, errResumeCursor
	}
	plain, err := c.aead.Open(nil, nil, sealed, []byte("mcp-response-v1"))
	if err != nil {
		return resumeCursor{}, errResumeCursor
	}
	defer clear(plain)
	var value resumeCursor
	if json.Unmarshal(plain, &value) != nil || !value.valid(now) {
		return resumeCursor{}, errResumeCursor
	}
	return value, nil
}

func WithResumeCursorCodec(codec *ResumeCursorCodec) Option {
	return func(h *Handler) { h.resumeCodec = codec }
}
