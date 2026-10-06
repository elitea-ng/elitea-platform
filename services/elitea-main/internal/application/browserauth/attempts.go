package browserauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBrowserAttemptClientKeyBytes = 512
	MaxBrowserAttemptRetryAfter     = 24 * time.Hour
)

var (
	ErrInvalidBrowserAttempt = errors.New("invalid browser authentication attempt")
	ErrAttemptLimited        = errors.New("browser authentication attempt limit reached")
)

type BrowserAttemptStage string

const (
	BrowserAttemptFormBegin      BrowserAttemptStage = "form_begin"
	BrowserAttemptFormCredential BrowserAttemptStage = "form_credential"
)

// BrowserAttempt contains only the bounded material needed for shared
// cross-replica admission. ClientKey is canonical trusted-proxy output, not a
// caller-selected forwarded header. LoginDigest is required only for Form
// credentials and must be a one-way digest of the submitted login.
type BrowserAttempt struct {
	ClientKey   string
	Stage       BrowserAttemptStage
	LoginDigest [sha256.Size]byte
}

func (attempt BrowserAttempt) Validate() error {
	if attempt.ClientKey == "" || len(attempt.ClientKey) > MaxBrowserAttemptClientKeyBytes ||
		!utf8.ValidString(attempt.ClientKey) || attempt.ClientKey != strings.TrimSpace(attempt.ClientKey) ||
		stringsContainControl(attempt.ClientKey) || strings.ContainsFunc(attempt.ClientKey, unicode.IsSpace) {
		return ErrInvalidBrowserAttempt
	}
	zeroDigest := attempt.LoginDigest == [sha256.Size]byte{}
	switch attempt.Stage {
	case BrowserAttemptFormCredential:
		if zeroDigest {
			return ErrInvalidBrowserAttempt
		}
	case BrowserAttemptFormBegin:
		if !zeroDigest {
			return ErrInvalidBrowserAttempt
		}
	default:
		return ErrInvalidBrowserAttempt
	}
	return nil
}

func stringsContainControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

// AttemptAdmitter is called before login state allocation and before expensive
// provider assertion work. Implementations must apply atomic stage-specific
// limits shared by all service replicas.
type AttemptAdmitter interface {
	Admit(context.Context, BrowserAttempt) (retryAfter time.Duration, err error)
}
