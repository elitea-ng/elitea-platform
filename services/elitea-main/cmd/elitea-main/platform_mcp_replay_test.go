package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	desktopopsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
)

// bootstrapPlatformReplayBucket creates the platform MCP replay bucket with
// the settings deploy/helm/elitea/files/nats-bootstrap.sh gives it, on a
// plaintext test server that has no bootstrap of its own.
func bootstrapPlatformReplayBucket(t *testing.T, conn *nats.Conn) {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: desktopopsapi.PlatformReplayBucket, History: 1, TTL: desktopopsapi.PlatformReplayTTL,
		MaxValueSize: desktopopsapi.PlatformReplayMaxValue, MaxBytes: 512 << 20, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("bootstrap the platform MCP replay bucket: %v", err)
	}
}

func TestPlatformMCPReplayStoreRequiresAConnection(t *testing.T) {
	t.Parallel()
	if store, err := newPlatformMCPReplayStore(context.Background(), nil); err == nil || store != nil {
		t.Fatalf("newPlatformMCPReplayStore(nil conn) = (%v, %v), want an error", store, err)
	}
}

// The boot path against a real JetStream server: the store binds to the
// bucket the bootstrap creates and claims a key exactly once.
func TestEventsNATSBootComposesThePlatformMCPReplayStore(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_NATS_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_NATS_URL (a JetStream-enabled NATS) to run the live-update boot test")
	}
	conn, err := newEventsNATSConn(envLookup(map[string]string{eventsNATSURLEnv: url}), nil)
	if err != nil || conn == nil {
		t.Fatalf("newEventsNATSConn() = (%v, %v), want a connection", conn, err)
	}
	t.Cleanup(conn.Close)
	bootstrapPlatformReplayBucket(t, conn)
	store, err := newPlatformMCPReplayStore(context.Background(), conn)
	if err != nil || store == nil {
		t.Fatalf("newPlatformMCPReplayStore() = (%v, %v), want a store", store, err)
	}
	ctx := context.Background()
	key := "call.boot-" + time.Now().Format("150405.000000000")
	first, err := store.Claim(ctx, key, "d")
	if err != nil || first.Verdict != desktopopsapi.ReplayClaimed {
		t.Fatalf("first claim = (%+v, %v)", first, err)
	}
	t.Cleanup(func() { _ = store.Release(context.Background(), key, first) })
	if second, err := store.Claim(ctx, key, "d"); err != nil || second.Verdict != desktopopsapi.ReplayInProgress {
		t.Fatalf("second claim = (%+v, %v), want in progress", second, err)
	}
}

// The composition root wires the KV store into the remote toolkit route. A
// nil (or per-replica) store at this seam is how a retried platform write
// would run twice on another replica, and no handler test composes main's
// wiring, so this reads it: main.go assigns platformMCPReplays from
// composePlatformMCPReplays and passes it to composeRemoteToolkit;
// composePlatformMCPReplays answers newPlatformMCPReplayStore's store (the
// live tests below prove it binds the bucket when it exists and degrades
// when it does not); composeRemoteToolkit sets
// RemoteToolkitDependencies.Replays from its parameter.
func TestProductionRemoteToolkitGetsTheNATSReplayStore(t *testing.T) {
	fileSet := token.NewFileSet()
	mainFile, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	assigned, passed := false, false
	ast.Inspect(mainFile, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 && identName(n.Lhs[0]) == "platformMCPReplays" {
				call, ok := n.Rhs[0].(*ast.CallExpr)
				assigned = assigned || (ok && identName(call.Fun) == "composePlatformMCPReplays")
			}
		case *ast.CallExpr:
			if identName(n.Fun) == "composeRemoteToolkit" {
				for _, arg := range n.Args {
					passed = passed || identName(arg) == "platformMCPReplays"
				}
			}
		}
		return true
	})
	if !assigned || !passed {
		t.Fatalf("main.go: platformMCPReplays assigned from composePlatformMCPReplays = %v, passed to composeRemoteToolkit = %v", assigned, passed)
	}

	natsFile, err := parser.ParseFile(fileSet, "event_stream_nats.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	fromStore := false
	ast.Inspect(natsFile, func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "composePlatformMCPReplays" {
			return true
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if assign, ok := node.(*ast.AssignStmt); ok && len(assign.Lhs) == 2 && identName(assign.Lhs[0]) == "store" {
				call, ok := assign.Rhs[0].(*ast.CallExpr)
				fromStore = fromStore || (ok && identName(call.Fun) == "newPlatformMCPReplayStore")
			}
			return true
		})
		return false
	})
	if !fromStore {
		t.Fatal("composePlatformMCPReplays no longer answers newPlatformMCPReplayStore's store")
	}

	opsFile, err := parser.ParseFile(fileSet, "desktop_ops.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(opsFile, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		selector, ok := literal.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RemoteToolkitDependencies" {
			return true
		}
		for _, element := range literal.Elts {
			if kv, ok := element.(*ast.KeyValueExpr); ok && identName(kv.Key) == "Replays" && identName(kv.Value) == "platformReplays" {
				wired = true
			}
		}
		return true
	})
	if !wired {
		t.Fatal("composeRemoteToolkit does not set RemoteToolkitDependencies.Replays from its platformReplays parameter")
	}
}

// Without the live-update NATS the store is a nil interface (never a typed
// nil, which the route would read as composed), and startup goes on.
func TestComposePlatformMCPReplaysWithoutNATSDegrades(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	if store := composePlatformMCPReplays(context.Background(), nil, slog.New(slog.NewTextHandler(&logs, nil))); store != nil {
		t.Fatalf("composePlatformMCPReplays(nil conn) = %#v, want a nil interface", store)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("no warning logged: %s", logs.String())
	}
}

// The boot composition against a real JetStream server: with the bucket it
// answers the KV store; with a bucket that cannot be bound (here: one too
// small for a stored answer, which newPlatformMCPReplayStore refuses exactly
// as it refuses a missing one) it logs an error naming the nats-bootstrap
// Job and answers a nil interface, so elitea-main starts and platform calls
// answer 501. The bucket is restored for the other tests.
func TestEventsNATSBootDegradesWithoutThePlatformMCPReplayBucket(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_NATS_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_NATS_URL (a JetStream-enabled NATS) to run the live-update boot test")
	}
	conn, err := newEventsNATSConn(envLookup(map[string]string{eventsNATSURLEnv: url}), nil)
	if err != nil || conn == nil {
		t.Fatalf("newEventsNATSConn() = (%v, %v), want a connection", conn, err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { bootstrapPlatformReplayBucket(t, conn) })

	for _, tc := range []struct {
		name    string
		prepare func()
	}{
		{"missing", func() {}},
		{"misconfigured", func() {
			if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket: desktopopsapi.PlatformReplayBucket, History: 1, TTL: time.Minute, Storage: jetstream.MemoryStorage,
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := js.DeleteKeyValue(ctx, desktopopsapi.PlatformReplayBucket); err != nil && !errors.Is(err, jetstream.ErrBucketNotFound) {
				t.Fatal(err)
			}
			tc.prepare()
			var logs bytes.Buffer
			store := composePlatformMCPReplays(ctx, conn, slog.New(slog.NewTextHandler(&logs, nil)))
			if store != nil {
				t.Fatalf("composePlatformMCPReplays() = %#v, want a nil interface", store)
			}
			if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "nats-bootstrap") ||
				!strings.Contains(logs.String(), desktopopsapi.PlatformReplayBucket) {
				t.Fatalf("the degraded store was not logged as an error naming the bootstrap Job: %s", logs.String())
			}
		})
	}

	_ = js.DeleteKeyValue(ctx, desktopopsapi.PlatformReplayBucket)
	bootstrapPlatformReplayBucket(t, conn)
	store := composePlatformMCPReplays(ctx, conn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := store.(*desktopopsapi.NATSPlatformReplays); !ok {
		t.Fatalf("composePlatformMCPReplays() with the bucket = %#v, want the KV store", store)
	}
}

// The resolved definition names each platform MCP server's tools from the
// bridge the router binds (issue 1231): main.go hands that bridge to the
// definition's service. Without it the definition offers a generic call with
// no tool names and cannot list a guardrail-blocked tool out.
func TestProductionResolvedVersionNamesPlatformMCPTools(t *testing.T) {
	mainFile, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(mainFile, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "WithPlatformTools" &&
			len(call.Args) == 1 && identName(call.Args[0]) == "platformMCPTools" {
			wired = true
		}
		return true
	})
	if !wired {
		t.Fatal("main.go does not give the resolved definition's service the platform MCP tool bridge (WithPlatformTools(platformMCPTools))")
	}
}

func identName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
