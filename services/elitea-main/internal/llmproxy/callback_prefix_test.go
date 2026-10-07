package llmproxy_test

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// The facade issues `callback-<uuid>` and the edge recognises it by the same
// prefix. Two constants keep material free of the edge; this pins them.
func TestCallbackExecutionPrefix(t *testing.T) {
	if material.CallbackExecutionPrefix != llmproxy.CallbackExecutionPrefix {
		t.Fatalf("material issues %q, the edge recognises %q",
			material.CallbackExecutionPrefix, llmproxy.CallbackExecutionPrefix)
	}
	block := material.CallbackSettings("https://main.example", material.Grant{Bearer: "b", UUID: "u-1"}, 42, "")
	if block["execution_id"] != llmproxy.CallbackExecutionPrefix+"u-1" {
		t.Fatalf("execution_id = %v", block["execution_id"])
	}
	if _, ok := material.CallbackSettings("https://main.example", material.Grant{Bearer: "b"}, 42, "")["execution_id"]; ok {
		t.Fatal("a grant without a uuid must not invent an execution id")
	}
}
