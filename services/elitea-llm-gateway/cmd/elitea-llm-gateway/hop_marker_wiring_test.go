package main

// hop_marker_wiring_test.go — the composition-root gates for hop-marker
// detection (issue #164).
//
// TestMainWiring already fails if hopmarker.New, llmproxy.WithHopMarker or
// logHopMarkerMode disappear. Three things it CANNOT see, each of which leaves
// the guard inert with every other test in the repository green:
//
//	1. the marker built from the WRONG key (cfg.IdentitySecret), which the
//	   issue rules out because the marker is published to tenant-authored
//	   upstreams;
//	2. the OUTBOUND half dropped — account.Config without HopMarker. Nothing
//	   then stamps a provider request, so the inbound half recognises nothing,
//	   forever, on every deployment;
//	3. two DIFFERENT markers wired to the two halves, which recognises nothing
//	   in exactly the same way.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
)

// parseMainGo parses main.go with its positions, so a node's source text can be
// recovered.
func parseMainGo(t *testing.T) (*token.FileSet, *ast.File, []byte) {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if perr != nil {
		t.Fatalf("parse main.go: %v", perr)
	}
	return fset, f, src
}

// TestHopMarkerBuiltFromItsOwnKey pins the key material.
//
// The marker rides every outbound provider request, and a provider api_base is
// tenant-authored, so the marker is published to addresses a tenant chooses.
// Building it from cfg.IdentitySecret would send a value derived from the key
// that signs the X-Elitea-* identity headers to those addresses, and would tie
// marker rotation to identity rotation. GATEWAY_HOP_SECRET is the only source.
func TestHopMarkerBuiltFromItsOwnKey(t *testing.T) {
	fset, f, src := parseMainGo(t)

	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "hopmarker" || sel.Sel.Name != "New" {
			return true
		}
		found++
		arg := string(src[fset.Position(call.Lparen).Offset:fset.Position(call.Rparen).Offset])
		if !strings.Contains(arg, "cfg.HopSecret") {
			t.Errorf("hopmarker.New is called with %q, which does not read cfg.HopSecret "+
				"(GATEWAY_HOP_SECRET). The marker must have dedicated key material.", arg)
		}
		if strings.Contains(arg, "IdentitySecret") {
			t.Errorf("hopmarker.New is called with %q, which reads the identity secret. "+
				"The marker is transmitted to every upstream and a provider api_base is "+
				"tenant-authored, so the key that signs the X-Elitea-* identity headers must "+
				"not travel there (issue #164).", arg)
		}
		return true
	})
	if found != 1 {
		t.Fatalf("main.go calls hopmarker.New %d times, want exactly 1. "+
			"Two markers built separately can be armed with different key material, and each half "+
			"would then recognise nothing the other sends.", found)
	}
}

// TestBothHopHalvesTakeTheSameMarker pins that the OUTBOUND stamp is wired,
// and wired to the same value as the inbound check.
//
// account.Config.HopMarker is the whole outbound half: without it
// GetConfigForProvider adds no header, so no request ever comes back marked and
// the inbound guard is a permanent no-op. A composite-literal field is invisible
// to TestMainWiring, which collects CALLS.
func TestBothHopHalvesTakeTheSameMarker(t *testing.T) {
	fset, f, src := parseMainGo(t)

	outbound := ""
	inbound := ""

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			sel, ok := node.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "account" || sel.Sel.Name != "Config" {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "HopMarker" {
					continue
				}
				outbound = string(src[fset.Position(kv.Value.Pos()).Offset:fset.Position(kv.Value.End()).Offset])
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "llmproxy" || sel.Sel.Name != "WithHopMarker" || len(node.Args) != 1 {
				return true
			}
			arg := node.Args[0]
			inbound = string(src[fset.Position(arg.Pos()).Offset:fset.Position(arg.End()).Offset])
		}
		return true
	})

	if outbound == "" {
		t.Fatal("account.Config in main.go sets no HopMarker field.\n" +
			"That is the OUTBOUND half: without it the gateway stamps no provider request, so no " +
			"request ever returns marked and the inbound guard refuses nothing, on every deployment. " +
			"A dropped composite-literal field is invisible to TestMainWiring, which sees calls only.")
	}
	if inbound == "" {
		t.Fatal("main.go passes no argument to llmproxy.WithHopMarker — the inbound half is not wired")
	}
	if outbound != inbound {
		t.Fatalf("the two halves take different markers:\n  outbound (account.Config.HopMarker): %s\n"+
			"  inbound  (llmproxy.WithHopMarker):   %s\n"+
			"Two markers with different key material recognise nothing the other stamps.",
			outbound, inbound)
	}
}

// TestHopSecretIsItsOwnEnvVar pins the read at the config boundary: setting the
// identity secret alone must leave hop detection unarmed, and setting the hop
// secret alone must not touch identity verification.
func TestHopSecretIsItsOwnEnvVar(t *testing.T) {
	t.Setenv("GATEWAY_IDENTITY_SECRET", "identity-only")
	t.Setenv("GATEWAY_HOP_SECRET", "")
	if got := mustConfigFromEnv(t).HopSecret; got != "" {
		t.Errorf("HopSecret = %q with only GATEWAY_IDENTITY_SECRET set, want \"\" — "+
			"the marker must never inherit the identity key", got)
	}

	t.Setenv("GATEWAY_HOP_SECRET", "hop-only")
	cfg := mustConfigFromEnv(t)
	if cfg.HopSecret != "hop-only" {
		t.Errorf("HopSecret = %q, want %q", cfg.HopSecret, "hop-only")
	}
	if cfg.IdentitySecret != "identity-only" {
		t.Errorf("IdentitySecret = %q, want %q — GATEWAY_HOP_SECRET must not disturb it",
			cfg.IdentitySecret, "identity-only")
	}
}

// TestLogHopMarkerMode covers the startup statement itself. "Never silently
// disarmed" is an acceptance criterion, and a startup log nobody asserts is a
// startup log that can be deleted or inverted without a failing test.
func TestLogHopMarkerMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		armed    bool
		wants    []string
		unwanted []string
	}{
		{
			name:     "unarmed says so, loudly",
			armed:    false,
			wants:    []string{"HOP DETECTION UNARMED", "GATEWAY_HOP_SECRET", `"level":"WARN"`},
			unwanted: []string{"HOP DETECTION ARMED"},
		},
		{
			name:     "armed says so, and names the header",
			armed:    true,
			wants:    []string{"HOP DETECTION ARMED", hopmarker.Header},
			unwanted: []string{"HOP DETECTION UNARMED"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logHopMarkerMode(slog.New(slog.NewJSONHandler(&buf, nil)), tc.armed)
			out := buf.String()
			for _, want := range tc.wants {
				if !strings.Contains(out, want) {
					t.Errorf("startup log does not contain %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(out, unwanted) {
					t.Errorf("startup log contains %q, which states the wrong mode:\n%s", unwanted, out)
				}
			}
		})
	}
}

// TestLogHopMarkerMode_NeverLogsKeyMaterial pins that the startup line carries
// no secret and no marker value. The marker reaches every upstream anyway, but
// a startup log is copied into tickets and chat threads that a provider request
// is not.
func TestLogHopMarkerMode_NeverLogsKeyMaterial(t *testing.T) {
	const secret = "a-very-recognisable-hop-secret"
	marker := hopmarker.New([]byte(secret))

	var buf bytes.Buffer
	logHopMarkerMode(slog.New(slog.NewJSONHandler(&buf, nil)), marker.Armed())

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Errorf("the startup log carries GATEWAY_HOP_SECRET in the clear:\n%s", out)
	}
	if strings.Contains(out, marker.Value()) {
		t.Errorf("the startup log carries the marker value:\n%s", out)
	}
}
