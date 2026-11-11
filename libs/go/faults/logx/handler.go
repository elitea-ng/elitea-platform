// Package logx is the services' shared slog handler (FU-299). It wraps the
// JSON handler each service already used and, without any call site
// changing:
//
//   - keeps "err" (or "error") as today's string, and adds the cause chain
//     made safe by type as "cause", and for a typed fault its "code",
//     "action" and bounded "stack" (Q4: existing queries on err keep working);
//   - adds the context's server-generated "support_ref" to every line.
package logx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
)

// Options configure the handler.
type Options struct {
	// Level is the service's existing level setting; nil means Info.
	Level slog.Leveler
	// Describe renders errors of known types safely (for example a
	// PostgreSQL error as SQLSTATE and message, without the DSN).
	Describe faults.Describer
}

// New returns a logger writing JSON lines to w through the handler.
func New(w io.Writer, opts Options) *slog.Logger {
	return slog.New(NewHandler(w, opts))
}

// NewHandler returns the handler.
func NewHandler(w io.Writer, opts Options) slog.Handler {
	return &handler{next: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: opts.Level}), describe: opts.Describe}
}

// Wrap adds the fault rendering and support_ref to an existing handler.
func Wrap(next slog.Handler, describe faults.Describer) slog.Handler {
	return &handler{next: next, describe: describe}
}

// LevelFromEnv reads the first of names that is set (debug, info, warn,
// error, case-insensitive) and falls back to Info, so a service that had
// no level variable keeps Info and one that had a variable keeps it.
func LevelFromEnv(lookup func(string) (string, bool), names ...string) slog.Level {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, name := range names {
		value, ok := lookup(name)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "debug":
			return slog.LevelDebug
		case "warn", "warning":
			return slog.LevelWarn
		case "error":
			return slog.LevelError
		default:
			return slog.LevelInfo
		}
	}
	return slog.LevelInfo
}

type handler struct {
	next     slog.Handler
	describe faults.Describer
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{next: h.next.WithAttrs(attrs), describe: h.describe}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{next: h.next.WithGroup(name), describe: h.describe}
}

func (h *handler) Handle(ctx context.Context, record slog.Record) error {
	out := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	hasSupportRef, hasCode := false, false
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "support_ref":
			hasSupportRef = true
		case "code":
			hasCode = true
		}
		return true
	})
	record.Attrs(func(attr slog.Attr) bool {
		err, isErr := errorValue(attr)
		if !isErr {
			out.AddAttrs(attr)
			return true
		}
		out.AddAttrs(slog.String(attr.Key, err.Error()))
		var fault *faults.Error
		if errors.As(err, &fault) && !hasCode {
			out.AddAttrs(slog.String("code", fault.Code().String()), slog.String("action", fault.Code().Action()))
			if stack := fault.Stack(); stack != "" {
				out.AddAttrs(slog.String("stack", stack))
			}
		}
		if chain := faults.CauseChain(err, h.describe); len(chain) > 1 || (len(chain) == 1 && chain[0] != err.Error()) {
			out.AddAttrs(slog.Any("cause", chain))
		}
		return true
	})
	if !hasSupportRef {
		if ref := faults.SupportRef(ctx); ref != "" {
			out.AddAttrs(slog.String("support_ref", ref))
		}
	}
	return h.next.Handle(ctx, out)
}

func errorValue(attr slog.Attr) (error, bool) {
	if attr.Key != "err" && attr.Key != "error" {
		return nil, false
	}
	value := attr.Value.Resolve()
	if value.Kind() != slog.KindAny {
		return nil, false
	}
	err, ok := value.Any().(error)
	return err, ok && err != nil
}
