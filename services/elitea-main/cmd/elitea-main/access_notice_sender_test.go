package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The sender's loop runs its pass and stops when the context ends.
func TestAccessNoticeSenderRunsAndStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{}, 1)
	backend := &downRegistry{}
	done := runAccessNoticeSender(ctx, backend, func(context.Context) (adminaccess.NoticeSendResult, error) {
		select {
		case ran <- struct{}{}:
		default:
		}
		return adminaccess.NoticeSendResult{Pending: 2}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the sender pass did not run")
	}
	if !backend.registered.Load() {
		t.Error("the loop did not register its routine")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sender loop did not stop when its context ended")
	}
}

// A nil pool composes nothing and is not an error.
func TestAccessNoticeSenderWithoutADatabase(t *testing.T) {
	if err := startAccessNoticeSender(context.Background(), nil, nil, "", slog.Default()); err != nil {
		t.Fatalf("startAccessNoticeSender(nil pool) = %v", err)
	}
}

// The composition root starts the sender with its own context, the composer
// and the public origin.
func TestCompositionRootStartsTheAccessNoticeSender(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "startAccessNoticeSender" || len(call.Args) != 5 {
			return true
		}
		names := make([]string, 0, len(call.Args))
		for _, arg := range call.Args {
			if name, ok := arg.(*ast.Ident); ok {
				names = append(names, name.Name)
			}
		}
		if len(names) == 5 && names[0] == "ctx" && names[2] == "mailComposer" && names[3] == "publicOrigin" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("main.go does not call startAccessNoticeSender(ctx, pool, mailComposer, publicOrigin, logger)")
	}
}

// The notice webhook (ADR-0059 R2.5b) is read at start: unset, the outbox
// stays e-mail only; set, its rows are turned on; half set or not https, the
// start is refused, with an error that does not name the URL.
func TestNoticeWebhookFromEnv(t *testing.T) {
	t.Cleanup(func() { adminaccess.SetNoticeWebhookEnabled(false) })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	secret := uuid.NewString()

	if webhook, err := noticeWebhookFromEnv(env(nil), logger); webhook != nil || err != nil ||
		adminaccess.NoticeWebhookEnabled() {
		t.Fatalf("unset: %v %v enabled=%v", webhook, err, adminaccess.NoticeWebhookEnabled())
	}
	for name, values := range map[string]map[string]string{
		"secret only": {adminaccess.NoticeWebhookSecretEnv: secret},
		"plain http": {adminaccess.NoticeWebhookURLEnv: "http://hooks.example/TOKEN",
			adminaccess.NoticeWebhookSecretEnv: secret},
		"weak secret": {adminaccess.NoticeWebhookURLEnv: "https://hooks.example/TOKEN",
			adminaccess.NoticeWebhookSecretEnv: "short"},
	} {
		webhook, err := noticeWebhookFromEnv(env(values), logger)
		if err == nil || webhook != nil || adminaccess.NoticeWebhookEnabled() {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
		if strings.Contains(err.Error(), "TOKEN") {
			t.Fatalf("%s: the refusal names the URL: %v", name, err)
		}
	}
	webhook, err := noticeWebhookFromEnv(env(map[string]string{
		adminaccess.NoticeWebhookURLEnv: "https://hooks.example/TOKEN", adminaccess.NoticeWebhookSecretEnv: secret,
	}), logger)
	if err != nil || webhook == nil || !adminaccess.NoticeWebhookEnabled() {
		t.Fatalf("set: %v %v enabled=%v", webhook, err, adminaccess.NoticeWebhookEnabled())
	}
}
