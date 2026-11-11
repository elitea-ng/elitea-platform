package api

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestComposedStoreLogsTheComposeError(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	type store struct{ name string }
	built := &store{name: "x"}
	if got := composedStore("demo", built, nil); got != built {
		t.Fatalf("a composed store was not passed through: %v", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("a successful compose logged: %s", buf.String())
	}
	if got := composedStore("demo", built, errors.New("audit appender: no pool")); got != nil {
		t.Fatalf("a failed compose kept its store: %v", got)
	}
	out := buf.String()
	if !strings.Contains(out, "store=demo") || !strings.Contains(out, "audit appender: no pool") {
		t.Fatalf("the compose error was not logged: %s", out)
	}
}
