package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	desktopopsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
)

// bootstrapPlatformReplayBucket creates the platform MCP replay bucket with
// the settings deploy/helm/nats-bootstrap/files/bootstrap.sh gives it, on a
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
// newPlatformMCPReplayStore and passes it to composeRemoteToolkit, which sets
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
				// platformMCPReplays = replayStore, where replayStore is
				// newPlatformMCPReplayStore's result (checked below).
				assigned = assigned || identName(n.Rhs[0]) == "replayStore"
			}
			if len(n.Rhs) == 1 && len(n.Lhs) == 2 && identName(n.Lhs[0]) == "replayStore" {
				call, ok := n.Rhs[0].(*ast.CallExpr)
				if !ok || identName(call.Fun) != "newPlatformMCPReplayStore" {
					t.Error("main.go's replayStore no longer comes from newPlatformMCPReplayStore")
				}
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
		t.Fatalf("main.go: platformMCPReplays assigned from the NATS store = %v, passed to composeRemoteToolkit = %v", assigned, passed)
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

func identName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
