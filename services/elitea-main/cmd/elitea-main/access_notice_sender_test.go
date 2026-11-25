package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"testing"
	"time"

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
