package toolkitcalltool

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// UnsupportedToolkitTypeError is the unrunnable-type refusal RunTool returns
// before any durable write. It matches ErrUnsupportedToolkitType under
// errors.Is.
//
// Error() keeps the operator form ("toolkit type cannot be run by this
// deployment: <type>: <reason>") for logs. A surface that shows the refusal
// to a PERSON reads UnsupportedToolkitTypeSentence instead: the capability
// reason is already a sentence (UI-DC-1), and wrapping it in the sentinel's
// prefix read as "...: ado_boards: This deployment's agent worker does not
// support the ado_boards toolkit." on the toolkit-run 422, and as
// "...toolkit.. Nothing was executed" in the MCP tool result.
type UnsupportedToolkitTypeError struct {
	ToolkitType string
	Reason      string
}

func (e *UnsupportedToolkitTypeError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrUnsupportedToolkitType, e.ToolkitType, e.Reason)
}

func (e *UnsupportedToolkitTypeError) Unwrap() error { return ErrUnsupportedToolkitType }

// genericUnsupportedSentence answers a bare ErrUnsupportedToolkitType, which
// carries no type and no reason (toolkit discovery returns it that way).
const genericUnsupportedSentence = "This deployment cannot run this toolkit type."

// UnsupportedToolkitTypeSentence is the refusal as one sentence for a person:
// the capability reason when err carries one, a generic sentence otherwise.
func UnsupportedToolkitTypeSentence(err error) string {
	var refusal *UnsupportedToolkitTypeError
	if errors.As(err, &refusal) {
		if sentence := AsSentence(refusal.Reason); sentence != "" {
			return sentence
		}
	}
	return genericUnsupportedSentence
}

// AsSentence trims a reason, capitalises its first letter and ends it with
// exactly one full stop, so a caller can place it between two sentences
// without doubling the period. It answers "" for a blank reason.
func AsSentence(reason string) string {
	text := strings.TrimSpace(reason)
	text = strings.TrimRight(text, ". ")
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	return string(unicode.ToUpper(first)) + text[size:] + "."
}
