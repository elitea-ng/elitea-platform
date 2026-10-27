package secretstore

import (
	"errors"
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// errSerialize is what every serialization of a RuntimeSecret returns.
var errSerialize = errors.New("secretstore: a runtime secret cannot be serialized")

// RuntimeSecret holds credential material. It formats as "[redacted]" under
// every fmt verb and in slog, and refuses JSON and text marshalling, so a
// value cannot reach a log line or a response body by accident. Reading the
// bytes takes the explicit UnsafeBytes call.
type RuntimeSecret struct {
	value []byte
}

// NewRuntimeSecret copies value. The caller should zero its own copy.
func NewRuntimeSecret(value []byte) RuntimeSecret {
	return RuntimeSecret{value: append([]byte(nil), value...)}
}

// UnsafeBytes returns the held bytes without copying. The caller must not
// retain them past Destroy and must not log them.
func (s RuntimeSecret) UnsafeBytes() []byte { return s.value }

// Len is the value length in bytes.
func (s RuntimeSecret) Len() int { return len(s.value) }

// Empty reports whether no value is held.
func (s RuntimeSecret) Empty() bool { return len(s.value) == 0 }

// Destroy zeroes the held bytes. Copies of the RuntimeSecret share them, so
// every copy reads zeroes afterwards.
func (s RuntimeSecret) Destroy() {
	for index := range s.value {
		s.value[index] = 0
	}
}

func (RuntimeSecret) String() string   { return redacted }
func (RuntimeSecret) GoString() string { return redacted }

// Format makes every verb, including %x and %#v, print the redaction.
func (RuntimeSecret) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(redacted)) }

// LogValue keeps the value out of structured logs.
func (RuntimeSecret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON refuses: a secret is never part of a JSON document.
func (RuntimeSecret) MarshalJSON() ([]byte, error) { return nil, errSerialize }

// MarshalText refuses for the same reason.
func (RuntimeSecret) MarshalText() ([]byte, error) { return nil, errSerialize }
