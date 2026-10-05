package storage

import (
	"strings"
	"testing"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

func codeFailureRouteFixture() (string, *NodeRecoveryFailureRoute) {
	instructions := "entry_point: run\nstate:\n  failure: {type: dict, value: {}}\n  answer: int\nnodes:\n  - id: run\n    type: code\n    language: python\n    code: '7'\n    input: []\n    output: [answer]\n    transition: END\n    recovery:\n      max_attempts: 1\n      on_failure:\n        route: handler\n        error_input: failure\n        classes: [attempt_timeout, dependency_unavailable]\n  - id: handler\n    type: code\n    language: python\n    code: '0'\n    input: [failure]\n    output: [answer]\n    transition: END\n    failure_handler: {error_input: failure}\n"
	// Source-derived cross-language framing vector. This test is not a Worker or
	// persisted checkpoint admission result; the owning Rust check remains separate.
	route := &NodeRecoveryFailureRoute{Schema: "elitea.pipeline.node-recovery-failure-route.v1", RouteID: "ec521fb97adbefc601b495af1841c508f33ec1c384265b1c4b78d1344ffb3524", Failed: NodeRecoveryFailedDescriptor{ActivationID: strings.Repeat("1", 64), Attempt: 1, FailureClass: "dependency_unavailable", StopReason: "attempts_exhausted"}}
	return instructions, route
}
func TestCodeFailureRouteMatchesDedicatedTypedHandlerAndWorkerTuple(t *testing.T) {
	instructions, route := codeFailureRouteFixture()
	handler, err := MatchCodeFailureRoute(instructions, code.Digest([]byte(instructions)), "run", route)
	if err != nil || handler != "handler" {
		t.Fatal("strict original dedicated handler refused", handler, err)
	}
	alias := strings.Replace(instructions, "  failure: {type: dict, value: {}}", "  failure: &failure_type {type: dict, value: {}}", 1)
	alias = strings.Replace(alias, "  answer: int", "  failure_copy: *failure_type\n  answer: int", 1)
	if handler, err = MatchCodeFailureRoute(alias, code.Digest([]byte(alias)), "run", route); err != nil || handler != "handler" {
		t.Fatal("bounded real state alias changed handler admission", handler, err)
	}
}
func TestCodeFailureRouteRejectsSuccessPathsOwnedNodesAndDenial(t *testing.T) {
	original, route := codeFailureRouteFixture()
	for _, name := range []string{"stale source", "forged route hash", "handler is entry", "wrong state type", "handler source dynamic", "handler writes error", "producer reads error", "handler has normal predecessor", "router default predecessor", "decision predecessor", "HITL predecessor", "Map owns handler", "Parallel owns handler", "denial class", "multi document"} {
		t.Run(name, func(t *testing.T) {
			instructions := original
			f := *route
			yamlSHA := ""
			switch name {
			case "stale source":
				yamlSHA = strings.Repeat("e", 64)
			case "forged route hash":
				f.RouteID = strings.Repeat("e", 64)
			case "handler is entry":
				instructions = strings.Replace(instructions, "entry_point: run", "entry_point: handler", 1)
			case "wrong state type":
				instructions = strings.Replace(instructions, "failure: {type: dict, value: {}}", "failure: list", 1)
			case "handler source dynamic":
				instructions = strings.Replace(instructions, "code: '0'", "code: {type: variable, value: answer}", 1)
			case "handler writes error":
				instructions = strings.Replace(instructions, "    failure_handler:", "    failure_handler:", 1)
				at := strings.LastIndex(instructions, "output: [answer]")
				instructions = instructions[:at] + strings.Replace(instructions[at:], "output: [answer]", "output: [failure]", 1)
			case "producer reads error":
				instructions = strings.Replace(instructions, "input: []", "input: [failure]", 1)
			case "handler has normal predecessor":
				instructions = strings.Replace(instructions, "transition: END", "transition: handler", 1)
			case "router default predecessor":
				instructions += "  - id: route\n    type: router\n    routes: [END]\n    default_output: handler\n"
			case "decision predecessor":
				instructions += "  - id: choose\n    type: decision\n    nodes: [handler]\n    default_output: END\n"
			case "HITL predecessor":
				instructions += "  - id: pause\n    type: hitl\n    routes: {approve: handler}\n"
			case "Map owns handler":
				instructions += "  - id: fan\n    type: map\n    worker: handler\n    transition: END\n"
			case "Parallel owns handler":
				instructions += "  - id: fan\n    type: parallel\n    branches: [{id: one, node: handler}, {id: two, node: run}]\n    transition: END\n"
			case "denial class":
				f.Failed.FailureClass = "authorization_denied"
			case "multi document":
				instructions += "---\nnodes: []\n"
			}
			if yamlSHA == "" {
				yamlSHA = code.Digest([]byte(instructions))
			}
			if handler, err := MatchCodeFailureRoute(instructions, yamlSHA, "run", &f); err == nil || handler != "" {
				t.Fatal("invalid handler path admitted", handler, err)
			}
		})
	}
}
func TestCodeFailureRouteDoesNotTreatStableParallelBranchLabelAsSuccessRoute(t *testing.T) {
	instructions, route := codeFailureRouteFixture()
	instructions += "  - id: fan\n    type: parallel\n    branches: [{id: handler, node: child_one}, {id: two, node: child_two}]\n    transition: END\n"
	if handler, err := MatchCodeFailureRoute(instructions, code.Digest([]byte(instructions)), "run", route); err != nil || handler != "handler" {
		t.Fatal("stable branch ID became routing authority", handler, err)
	}
}
