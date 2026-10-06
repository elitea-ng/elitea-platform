package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"
)

type recordingSink struct {
	mu    sync.Mutex
	types []string
	done  chan struct{}
}

func (s *recordingSink) HandleDomainEvent(_ context.Context, _, eventType string, _ any) {
	s.mu.Lock()
	s.types = append(s.types, eventType)
	s.mu.Unlock()
	close(s.done)
}

// The publisher still delivers to its webhook sinks: dropping the live bus
// must not drop webhooks.
func TestDomainEventsPublisherReachesItsSinks(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{done: make(chan struct{})}
	newDomainEventsPublisher(sink).Emit(context.Background(), "7", "conversation.created", map[string]string{"name": "x"})
	select {
	case <-sink.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook sink never received the domain event")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.types) != 1 || sink.types[0] != "conversation.created" {
		t.Fatalf("sink received %v, want [conversation.created]", sink.types)
	}
}

// main.go must build domainEvents through newDomainEventsPublisher (which has
// no Bus parameter), never through events.NewPublisher directly: the direct
// call is how the live NATS bus reached the project SSE stream, exposing
// private conversation names to project viewers (PR #1074 review).
func TestDomainEventsStayOffTheLiveBus(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var assigned bool
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name != "domainEvents" || i >= len(n.Rhs) {
					continue
				}
				var fnName string
				if call, ok := n.Rhs[i].(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok {
						fnName = fn.Name
					}
				}
				if fnName != "newDomainEventsPublisher" {
					t.Errorf("%s: domainEvents is not built by newDomainEventsPublisher", fileSet.Position(n.Pos()))
				}
				assigned = true
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewPublisher" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "events" {
					t.Errorf("%s: main.go calls events.NewPublisher directly; "+
						"use newDomainEventsPublisher so domain events cannot reach the live bus",
						fileSet.Position(n.Pos()))
				}
			}
		}
		return true
	})
	if !assigned {
		t.Fatal("main.go no longer assigns domainEvents; update this guard")
	}
}
