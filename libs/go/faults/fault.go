package faults

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Bounds on what one fault line carries (FU-299 C2).
const (
	maxCauseDepth      = 8
	maxCauseBytes      = 512
	maxStackFrames     = 32
	maxStackBytes      = 8 << 10
	stackRateLimitSpan = time.Second
)

// Error is a typed failure: a registered code, the ids that identify what
// failed, the cause, and for unexpected failures a bounded stack.
type Error struct {
	code  Code
	cause error
	attrs []slog.Attr
	stack string
}

// New is an expected failure with a registered code.
func New(code Code, cause error, ids ...slog.Attr) *Error {
	return &Error{code: code, cause: cause, attrs: ids}
}

// Unexpected is a failure nobody planned for, reaching a boundary (an error
// writer's default branch). It carries a bounded stack of where it was
// raised, at most one stack per code per second.
func Unexpected(cause error, ids ...slog.Attr) *Error {
	return &Error{code: InternalUnexpected, cause: cause, attrs: ids, stack: captureStack(InternalUnexpected, 2)}
}

// Panic is a recovered panic. The panic value is never rendered: only its
// type is kept, so a value holding data cannot reach a log or a caller.
func Panic(recovered any, ids ...slog.Attr) *Error {
	cause := fmt.Errorf("panic with a value of type %T", recovered)
	return &Error{code: InternalPanic, cause: cause, attrs: ids, stack: captureStack(InternalPanic, 3)}
}

// Code is the registered code.
func (e *Error) Code() Code { return e.code }

// Error is the sentence and the cause, so %w callers keep working.
func (e *Error) Error() string {
	if e.cause == nil {
		return e.code.Sentence()
	}
	return e.code.Sentence() + ": " + e.cause.Error()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.cause }

// IDs are the attributes that identify what failed.
func (e *Error) IDs() []slog.Attr { return e.attrs }

// Stack is the bounded stack of an unexpected failure or a panic: empty for
// an expected failure, "rate-limited" when another stack for the same code
// was taken less than a second ago.
func (e *Error) Stack() string { return e.stack }

// Log writes err as one fault line at level: the code's sentence as the
// message, with the code, action, ids, support_ref and cause added by the
// logx handler.
func Log(ctx context.Context, logger *slog.Logger, level slog.Level, err *Error) {
	args := make([]any, 0, 8+len(err.attrs))
	args = append(args, "err", err, "code", err.code.String(), "action", err.code.Action())
	if err.stack != "" {
		args = append(args, "stack", err.stack)
	}
	for _, attr := range err.attrs {
		args = append(args, attr)
	}
	logger.Log(ctx, level, err.code.Sentence(), args...)
}

// Describer renders one error of a known type safely (for example a
// PostgreSQL error as its SQLSTATE and message, without connection
// details). It reports false for a type it does not know.
type Describer func(error) (string, bool)

// CauseChain is err's cause chain, outermost first, at most 8 elements of at
// most 512 bytes. A typed fault renders as "<code>: <sentence>"; a type the
// describer knows renders as it says; anything else keeps its own text,
// which is what the flat err string logged today already carries.
func CauseChain(err error, describe Describer) []string {
	var chain []string
	for current := err; current != nil && len(chain) < maxCauseDepth; {
		var text string
		var fault *Error
		switch {
		case errors.As(current, &fault) && fault == current:
			text = fault.code.String() + ": " + fault.code.Sentence()
		default:
			if describe != nil {
				if described, ok := describe(current); ok {
					text = described
					chain = append(chain, bound(text))
					return chain // a described error stands for its whole tail
				}
			}
			text = ownText(current)
		}
		if text != "" {
			chain = append(chain, bound(text))
		}
		current = next(current)
	}
	return chain
}

// ownText is current's text without the text of the error it wraps, so the
// chain does not repeat itself.
func ownText(err error) string {
	text := err.Error()
	if inner := next(err); inner != nil {
		text = strings.TrimSuffix(text, inner.Error())
		text = strings.TrimSuffix(strings.TrimSpace(text), ":")
	}
	return strings.TrimSpace(text)
}

func next(err error) error {
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		return wrapped.Unwrap()
	case interface{ Unwrap() []error }:
		if errs := wrapped.Unwrap(); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

func bound(text string) string {
	if len(text) <= maxCauseBytes {
		return text
	}
	cut := maxCauseBytes
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

var (
	stackMu   sync.Mutex
	lastStack = map[Code]time.Time{}
	nowFunc   = time.Now
)

func captureStack(code Code, skip int) string {
	stackMu.Lock()
	now := nowFunc()
	if last, ok := lastStack[code]; ok && now.Sub(last) < stackRateLimitSpan {
		stackMu.Unlock()
		return "rate-limited"
	}
	lastStack[code] = now
	stackMu.Unlock()

	pcs := make([]uintptr, maxStackFrames)
	n := runtime.Callers(skip+1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		frame, more := frames.Next()
		line := fmt.Sprintf("%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		if b.Len()+len(line) > maxStackBytes {
			b.WriteString("…\n")
			break
		}
		b.WriteString(line)
		if !more {
			break
		}
	}
	return b.String()
}
