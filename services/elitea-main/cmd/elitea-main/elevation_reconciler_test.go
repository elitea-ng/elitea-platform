package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// downRegistry is a routine registry that cannot be reached: the loop then
// runs the pass at once on its local clock, as on a fresh process.
type downRegistry struct{ registered atomic.Bool }

var errRegistryDown = errors.New("registry down")

func (d *downRegistry) Register(context.Context, ...routines.Definition) error {
	d.registered.Store(true)
	return errRegistryDown
}
func (*downRegistry) Get(context.Context, string) (routines.Row, error) {
	return routines.Row{}, errRegistryDown
}
func (*downRegistry) Acquire(context.Context, string, *time.Time, time.Time, string, time.Duration) (routines.Lease, bool, error) {
	return routines.Lease{}, false, errRegistryDown
}
func (*downRegistry) Renew(context.Context, string, routines.Lease, time.Duration) error {
	return errRegistryDown
}
func (*downRegistry) Release(context.Context, string, routines.Lease, routines.Completion) error {
	return errRegistryDown
}

// The reconciler's loop runs its pass and stops when the context ends: the
// shutdown of the composition root.
func TestElevationReconcilerRunsAndStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{}, 1)
	backend := &downRegistry{}
	done := runElevationReconciler(ctx, backend, func(context.Context) (adminaccess.ReconcileResult, error) {
		select {
		case ran <- struct{}{}:
		default:
		}
		return adminaccess.ReconcileResult{Expired: 1}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconciler pass did not run")
	}
	if !backend.registered.Load() {
		t.Error("the loop did not register its routine")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconciler loop did not stop when its context ended")
	}
}

// A nil pool composes nothing and is not an error.
func TestElevationReconcilerWithoutADatabase(t *testing.T) {
	done, err := startElevationReconciler(context.Background(), nil, slog.Default())
	if done != nil || err != nil {
		t.Fatalf("startElevationReconciler(nil pool) = %v, %v", done, err)
	}
}

// The composition root starts the reconciler, with its own context.
func TestCompositionRootStartsTheElevationReconciler(t *testing.T) {
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
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "startElevationReconciler" && len(call.Args) == 3 {
			if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == "ctx" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go does not call startElevationReconciler(ctx, ...)")
	}
}
